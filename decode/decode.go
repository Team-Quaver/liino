// Package decode defines the decoder boundary of the Liino pipeline.
//
// A Backend turns a Source into a Stream of canonical PCM: interleaved
// float32 at 48 kHz, keeping the source's channel count (see package pcm).
// Everything downstream - DSP, buffering, device output - only ever sees that
// shape, so adding a decoder never touches the playback engine. The player
// folds channels to the endpoint's layout when they differ; decoders must not
// downmix themselves, because swresample cannot rematrix layouts it does not
// know (7.1.4 Opus reports 12 channels with no layout).
//
// Two backends ship with Liino:
//
//   - native: ffgo, pure Go bindings that dlopen the system FFmpeg
//     (libavformat/libavcodec/libavutil/libswresample) through purego. No CGO,
//     no bundled FFmpeg binaries.
//   - wasm: a libav.js helper process (Node) that emits PCM over a pipe, used
//     where no FFmpeg is available and a bundled runtime is acceptable.
package decode

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/team-quaver/liino/pcm"
)

// Canonical format produced by every decoder.
var Canonical = pcm.DefaultFormat()

// Errors shared by decoder backends.
var (
	// ErrNoBackend is returned when no decoder backend can run in this process.
	ErrNoBackend = errors.New("decode: no decoder backend available")
	// ErrUnavailable is returned by a backend whose runtime is missing.
	ErrUnavailable = errors.New("decode: backend unavailable")
	// ErrSeekUnsupported is returned when a stream cannot seek.
	ErrSeekUnsupported = errors.New("decode: seek not supported")
	// ErrNotOpen is returned for operations on a closed stream.
	ErrNotOpen = errors.New("decode: stream not open")
	// ErrTimeout reports a probe/open timeout.
	ErrTimeout = errors.New("decode: open timeout")
	// ErrBadFormat is returned when neither the container nor the decoded
	// frames reveal a usable sample rate and channel count. Resampling cannot
	// start without them, so failing loudly beats feeding garbage downstream.
	ErrBadFormat = errors.New("decode: cannot determine source audio format")
)

// Source describes what to decode.
//
// Exactly one of URI or Reader is used. URI accepts absolute paths, file://
// URLs, and whatever the underlying container demuxer understands (http(s),
// tcp, pipe:). Reader is for hosts that already hold the byte stream - Quaver
// resolves signed cloud-drive URLs in Node and pipes the bytes down, so the
// decoder never needs to know about cookies or range headers.
type Source struct {
	URI     string
	Reader  io.Reader
	Format  string            // container hint, e.g. "mp3", "flac"
	Options map[string]string // backend specific knobs (see Options)
	Title   string
}

// IsStream reports whether the source is a pull-based byte stream.
func (s Source) IsStream() bool { return s.Reader != nil }

// IsLive reports whether the source is a radio-style endless stream.
func (s Source) IsLive() bool {
	switch {
	case s.Options["live"] == "1", s.Options["live"] == "true":
		return true
	case strings.HasPrefix(s.URI, "udp://"), strings.HasSuffix(s.URI, ".m3u8"):
		return true
	default:
		return false
	}
}

// Meta describes a decoded stream.
type Meta struct {
	Codec      string
	BitRate    int64
	SampleRate int
	Channels   int
	Duration   time.Duration // 0 when unknown (live streams)
	Seekable   bool
	Live       bool
}

// Stream is an open decoder session. Implementations are not safe for
// concurrent use; the engine owns exactly one goroutine per stream.
type Stream interface {
	// Format is canonical 48 kHz float32 at the source channel count.
	Format() pcm.Format
	// Meta returns container information valid from Open onwards.
	Meta() Meta
	// Read returns the next chunk of PCM, or io.EOF at the end of the stream.
	Read() (pcm.Chunk, error)
	// Seek moves to t and reports the position actually reached.
	Seek(t time.Duration) (time.Duration, error)
	// Close releases the decoder.
	Close() error
}

// Options tunes opening behaviour. Zero values mean backend defaults.
type Options struct {
	// ProbeBytes limits how many bytes the demuxer may inspect.
	ProbeBytes int
	// AnalyzeDuration bounds stream analysis for stubborn containers.
	AnalyzeDuration time.Duration
	// OpenTimeout bounds the whole Open call; 0 means no limit.
	OpenTimeout time.Duration
	// ChunkFrames is the preferred frame count per returned chunk.
	ChunkFrames int
	// Log receives backend diagnostics when non-nil.
	Log func(string)
}

// Backend is one decoder implementation.
type Backend interface {
	// Name is the stable identifier used in config and events.
	Name() string
	// Priority orders backends for the default pick (lower wins).
	Priority() int
	// Probe reports whether the backend can decode in this process right now,
	// plus a human-readable note for the UI and logs.
	Probe() (ok bool, note string)
	// Open starts decoding src. ctx cancels a slow open.
	Open(ctx context.Context, src Source, opts Options) (Stream, error)
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Backend{}
)

// Register makes a backend available process-wide. Backends call it from init.
func Register(b Backend) {
	if b == nil {
		return
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[b.Name()] = b
}

// Unregister removes a backend (tests only).
func Unregister(name string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(registry, name)
}

// Lookup finds a backend by name.
func Lookup(name string) (Backend, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	b, ok := registry[name]
	return b, ok
}

// Backends lists every registered backend sorted by priority then name.
func Backends() []Backend {
	registryMu.RLock()
	out := make([]Backend, 0, len(registry))
	for _, b := range registry {
		out = append(out, b)
	}
	registryMu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority() != out[j].Priority() {
			return out[i].Priority() < out[j].Priority()
		}
		return out[i].Name() < out[j].Name()
	})
	return out
}

// Default returns the highest-priority backend that can actually run.
func Default() (Backend, error) {
	for _, b := range Backends() {
		if ok, _ := b.Probe(); ok {
			return b, nil
		}
	}
	return nil, ErrNoBackend
}

// Status describes one backend for diagnostics.
type Status struct {
	Name      string `json:"name"`
	Priority  int    `json:"priority"`
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`
}

// Statuses reports all backends, for the settings UI and the doctor command.
func Statuses() []Status {
	out := make([]Status, 0, len(registry))
	for _, b := range Backends() {
		ok, note := b.Probe()
		out = append(out, Status{Name: b.Name(), Priority: b.Priority(), Available: ok, Note: note})
	}
	return out
}

// OpenWith opens src on the named backend, falling back to the default one
// when name is empty.
func OpenWith(ctx context.Context, name string, src Source, opts Options) (Stream, Backend, error) {
	b, err := resolve(name)
	if err != nil {
		return nil, nil, err
	}
	if opts.OpenTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.OpenTimeout)
		defer cancel()
	}
	st, err := b.Open(ctx, src, opts)
	if err != nil {
		return nil, b, err
	}
	return st, b, nil
}

func resolve(name string) (Backend, error) {
	if name == "" {
		return Default()
	}
	b, ok := Lookup(name)
	if !ok {
		return nil, ErrNoBackend
	}
	if ok, note := b.Probe(); !ok {
		if note != "" {
			return nil, errors.New(note)
		}
		return nil, ErrUnavailable
	}
	return b, nil
}

// Label returns a short, log-safe description of the source.
func (s Source) Label() string {
	switch {
	case s.Title != "":
		return s.Title
	case s.URI != "":
		return s.URI
	case s.Reader != nil:
		return "stream"
	default:
		return "<empty>"
	}
}
