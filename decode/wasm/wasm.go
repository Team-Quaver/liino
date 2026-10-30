// Package wasm decodes audio through a libav.js helper process.
//
// This is the fallback path, not the preferred one. Liino's primary decoder is
// the native ffgo backend; running FFmpeg as WASM inside Node costs latency,
// single-core throughput and memory, and cannot reach the device APIs. It stays
// here for builds that must not depend on system FFmpeg libraries at all.
//
// The wire protocol is deliberately tiny:
//
//   - Go -> helper: newline-delimited JSON commands on stdin.
//   - helper -> Go: framed binary on stdout, 4-byte big-endian payload length,
//     1 byte kind ('J' JSON event, 'P' little-endian float32 interleaved PCM).
//
// The helper emits whatever the container holds (rate, channel count, float32);
// this package resamples and downmixes into the canonical pipeline format so
// the JS side stays a decoder and nothing more.
package wasm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/team-quaver/liino/decode"
	"github.com/team-quaver/liino/pcm"
)

// Name is the registry key of this backend.
const Name = "libavjs"

// Config locates the Node runtime and the helper script.
type Config struct {
	Node   string   // node binary, default "node"
	Script string   // path to decoder.mjs
	Env    []string // extra environment entries
	Dir    string   // working directory for the helper
}

var (
	cfgMu sync.RWMutex
	cfg   = Config{Node: "node"}
)

// SetConfig points the backend at a Node runtime and helper script. Electron
// hosts set this to their bundled node binary and the packaged decoder.mjs.
func SetConfig(c Config) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if c.Node == "" {
		c.Node = "node"
	}
	cfg = c
}

// ConfigSnapshot reports the current helper configuration.
func ConfigSnapshot() Config {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg
}

func currentConfig() Config {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	c := cfg
	if c.Script == "" {
		c.Script = defaultScript()
	}
	return c
}

func defaultScript() string {
	if env := os.Getenv("LIINO_LIBAVJS_DECODER"); env != "" {
		return env
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "liino", "decoder.mjs")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return filepath.Join("decode", "wasm", "node", "decoder.mjs")
}

type backend struct{}

func init() { decode.Register(backend{}) }

func (backend) Name() string  { return Name }
func (backend) Priority() int { return 10 }

func (backend) Probe() (bool, string) {
	c := currentConfig()
	bin := c.Node
	if bin == "" {
		bin = "node"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return false, "node runtime not found: " + bin
	}
	if c.Script == "" {
		return false, "libav.js helper script not configured"
	}
	if _, err := os.Stat(c.Script); err != nil {
		return false, "helper script missing: " + c.Script
	}
	return true, "node " + path
}

// Open spawns the helper and starts decoding.
func (backend) Open(ctx context.Context, src decode.Source, opts decode.Options) (decode.Stream, error) {
	if src.URI == "" {
		return nil, errors.New("decode wasm: URI is required (the helper fetches and decodes)")
	}
	c := currentConfig()
	bin := c.Node
	if bin == "" {
		bin = "node"
	}
	args := []string{c.Script, "--ipc"}
	if src.Format != "" {
		args = append(args, "--format", src.Format)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), c.Env...)
	if c.Dir != "" {
		cmd.Dir = c.Dir
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("decode wasm: start helper: %w", err)
	}

	s := &stream{
		cmd:    cmd,
		in:     stdin,
		out:    newFramer(stdout),
		log:    opts.Log,
		closed: make(chan struct{}),
		errs:   make(chan error, 1),
	}
	go s.pumpStderr(errPipe)

	req := map[string]any{"cmd": "open", "uri": src.URI}
	if src.Format != "" {
		req["format"] = src.Format
	}
	if err := s.send(req); err != nil {
		_ = s.Close()
		return nil, err
	}

	var meta struct {
		Event    string  `json:"event"`
		Rate     int     `json:"sample_rate"`
		Channels int     `json:"channels"`
		Duration float64 `json:"duration"`
		Codec    string  `json:"codec"`
		Seekable bool    `json:"seekable"`
		Error    string  `json:"error"`
	}
	openCtx := ctx
	if opts.OpenTimeout > 0 {
		var cancel context.CancelFunc
		openCtx, cancel = context.WithTimeout(ctx, opts.OpenTimeout)
		defer cancel()
	}
	if err := s.readMeta(openCtx, &meta); err != nil {
		_ = s.Close()
		return nil, err
	}
	if meta.Error != "" {
		_ = s.Close()
		return nil, errors.New("decode wasm: " + meta.Error)
	}
	if meta.Rate <= 0 {
		meta.Rate = pcm.DefaultSampleRate
	}
	if meta.Channels <= 0 {
		meta.Channels = pcm.DefaultChannels
	}
	s.native = pcm.Format{SampleRate: meta.Rate, Channels: meta.Channels}
	s.meta = decode.Meta{
		Codec:      meta.Codec,
		SampleRate: meta.Rate,
		Channels:   meta.Channels,
		Duration:   time.Duration(meta.Duration * float64(time.Second)),
		Seekable:   meta.Seekable,
		Live:       src.IsLive(),
	}
	if s.meta.Live {
		s.meta.Duration = 0
		s.meta.Seekable = false
	}
	s.chunk = pcm.Chunk{Data: make([]float32, maxInt(opts.ChunkFrames, pcm.ChunkFrames)*pcm.DefaultChannels), Format: pcm.DefaultFormat()}
	return s, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// stream is one helper process plus its PCM conversion state.
type stream struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *framer
	meta   decode.Meta
	native pcm.Format
	chunk  pcm.Chunk
	stereo []float32 // native-rate stereo staging
	log    func(string)

	mu     sync.Mutex
	closed chan struct{}
	errs   chan error
	done   bool
}

func (s *stream) Format() pcm.Format { return pcm.DefaultFormat() }
func (s *stream) Meta() decode.Meta  { return s.meta }

// Read returns the next canonical chunk.
func (s *stream) Read() (pcm.Chunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closed:
		return pcm.Chunk{}, decode.ErrNotOpen
	default:
	}
	for {
		payload, kind, err := s.out.read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if serr := s.waitErr(); serr != nil {
					return pcm.Chunk{}, serr
				}
				return pcm.Chunk{}, io.EOF
			}
			return pcm.Chunk{}, err
		}
		switch kind {
		case kindPCM:
			native := pcm.BytesF32(payload)
			frames := len(native) / s.native.Channels
			if frames == 0 {
				continue
			}
			out := s.convert(native[:frames*s.native.Channels], frames)
			if out == 0 {
				continue
			}
			s.chunk.Frames = out
			return s.chunk, nil
		case kindJSON:
			var ev map[string]any
			if err := json.Unmarshal(payload, &ev); err != nil {
				continue
			}
			switch ev["event"] {
			case "eof":
				if serr := s.waitErr(); serr != nil {
					return pcm.Chunk{}, serr
				}
				return pcm.Chunk{}, io.EOF
			case "error":
				return pcm.Chunk{}, fmt.Errorf("decode wasm: %v", ev["error"])
			default:
				continue
			}
		}
	}
}

// convert resamples and downmixes native frames into the canonical chunk. The
// chunk buffer grows to fit: dropping decoded audio is never acceptable, so the
// output is sized from the exact rate ratio.
//
// The pure-Go polyphase filter is stateless, so block boundaries cost a few
// samples of continuity. That drift is why this backend is the fallback: the
// native ffgo path uses libswresample, which is stateful and better sounding.
func (s *stream) convert(native []float32, frames int) int {
	inRate := s.native.SampleRate
	outRate := pcm.DefaultSampleRate
	stereo := native
	if s.native.Channels != pcm.DefaultChannels {
		s.stereo = ensureCap(s.stereo, frames*pcm.DefaultChannels)
		pcm.DownmixChannels(s.stereo[:frames*pcm.DefaultChannels], native, frames, s.native.Channels)
		stereo = s.stereo[:frames*pcm.DefaultChannels]
	}

	want := frames
	if inRate != outRate {
		want = (frames*outRate + inRate/2) / inRate
		s.chunk.Data = ensureCap(s.chunk.Data, want*pcm.DefaultChannels)
		pcm.ResamplePoly(s.chunk.Data[:want*pcm.DefaultChannels], stereo, inRate, outRate, pcm.DefaultChannels, 32)
		return want
	}
	s.chunk.Data = ensureCap(s.chunk.Data, want*pcm.DefaultChannels)
	copy(s.chunk.Data[:want*pcm.DefaultChannels], stereo[:want*pcm.DefaultChannels])
	return want
}

// Seek asks the helper to reposition.
func (s *stream) Seek(t time.Duration) (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.meta.Seekable {
		return 0, decode.ErrSeekUnsupported
	}
	if err := s.send(map[string]any{"cmd": "seek", "seconds": t.Seconds()}); err != nil {
		return 0, err
	}
	s.stereo = s.stereo[:0]
	var ev struct {
		Event   string  `json:"event"`
		Seconds float64 `json:"seconds"`
		Error   string  `json:"error"`
	}
	for {
		payload, kind, err := s.out.read()
		if err != nil {
			return 0, err
		}
		if kind != kindJSON {
			continue
		}
		if err := json.Unmarshal(payload, &ev); err != nil {
			continue
		}
		if ev.Event == "seeked" || ev.Event == "error" {
			break
		}
	}
	if ev.Error != "" {
		return 0, errors.New("decode wasm: " + ev.Error)
	}
	return time.Duration(ev.Seconds * float64(time.Second)), nil
}

// Close stops the helper process.
func (s *stream) Close() error {
	s.mu.Lock()
	select {
	case <-s.closed:
		s.mu.Unlock()
		return nil
	default:
	}
	close(s.closed)
	_ = s.send(map[string]any{"cmd": "close"})
	s.mu.Unlock()

	if err := s.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return s.cmd.Wait()
	}
	return s.cmd.Wait()
}

func (s *stream) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.in.Write(append(data, '\n'))
	return err
}

func (s *stream) readMeta(ctx context.Context, v any) error {
	done := make(chan error, 1)
	go func() {
		payload, kind, err := s.out.read()
		if err != nil {
			done <- err
			return
		}
		if kind != kindJSON {
			done <- errors.New("decode wasm: expected meta event")
			return
		}
		done <- json.Unmarshal(payload, v)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%w: %v", decode.ErrTimeout, ctx.Err())
	case <-s.closed:
		return decode.ErrNotOpen
	}
}

func (s *stream) waitErr() error {
	select {
	case err := <-s.errs:
		return err
	default:
		return nil
	}
}

func (s *stream) pumpStderr(r io.Reader) {
	buf := make([]byte, 4096)
	br := bufio.NewReaderSize(r, 4096)
	for {
		line, err := br.ReadString('\n')
		if line != "" && s.log != nil {
			s.log("libavjs: " + trimLine(line))
		}
		if err != nil {
			return
		}
		_ = buf
	}
}

func trimLine(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func ensureCap(b []float32, n int) []float32 {
	if cap(b) >= n {
		return b[:n]
	}
	return make([]float32, n)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
