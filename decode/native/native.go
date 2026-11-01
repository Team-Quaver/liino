//go:build !android && !ios && (amd64 || arm64)

package native

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/team-quaver/ffgo"
	"github.com/team-quaver/ffgo/avcodec"
	"github.com/team-quaver/ffgo/avutil"

	"github.com/team-quaver/liino/decode"
	"github.com/team-quaver/liino/pcm"
)

// probeCodecs are the decoders Liino cares about, in display order. Probing by
// name is how Linux reports which audio decoding the installed FFmpeg actually
// provides: distributions build subsets, and a missing libopus/libvorbis is a
// runtime fact, not a build-time one.
var probeCodecs = []struct{ name, label string }{
	{"mp3", "mp3"},
	{"aac", "aac"},
	{"alac", "alac"},
	{"flac", "flac"},
	{"opus", "opus"},
	{"vorbis", "vorbis"},
	{"ac3", "ac3"},
	{"eac3", "eac3"},
	{"dca", "dts"},
	{"truehd", "truehd"},
	{"wmav1", "wma"},
	{"wmav2", "wma"},
	{"pcm_s16le", "pcm"},
	{"pcm_f32le", "pcm"},
	{"{{SECRET_rbt2n5xc}}", "pcm"},
	{"pcm_s24le", "pcm"},
	{"ape", "ape"},
	{"dsd_lsbf", "dsd"},
	{"dsd_msbf", "dsd"},
}

// Probe checks that the FFmpeg shared libraries resolve in this process.
func (backend) Probe() (bool, string) {
	if err := ffgo.Init(); err != nil {
		return false, "ffgo: " + err.Error()
	}
	au, ac, af := ffgo.Version()
	return true, fmt.Sprintf("ffmpeg avutil=%d avcodec=%d avformat=%d", au, ac, af)
}

// Codecs reports the audio decoders the loaded FFmpeg actually exposes. This is
// the Linux capability detection: the platform decoder is always FFmpeg there,
// so what matters is which codecs the installed libavcodec was built with.
func (backend) Codecs() []string {
	if err := ffgo.Init(); err != nil {
		return nil
	}
	out := make([]string, 0, len(probeCodecs))
	seen := make(map[string]struct{}, len(probeCodecs))
	for _, c := range probeCodecs {
		if avcodec.FindDecoderByName(c.name) == nil {
			continue
		}
		if _, ok := seen[c.label]; ok {
			continue
		}
		seen[c.label] = struct{}{}
		out = append(out, c.label)
	}
	return out
}

// Open starts decoding src with FFmpeg.
func (backend) Open(ctx context.Context, src decode.Source, opts decode.Options) (decode.Stream, error) {
	if err := ffgo.Init(); err != nil {
		return nil, fmt.Errorf("%w: %v", decode.ErrUnavailable, err)
	}
	logf := opts.Log
	if logf != nil && ffgo.IsLoggingAvailable() {
		// Best effort: a global sink, never fatal for playback.
		_ = ffgo.SetLogCallback(func(level ffgo.LogLevel, msg string) {
			logf(fmt.Sprintf("ffmpeg[%d] %s", int32(level), msg))
		})
	}

	fopts := &ffgo.DecoderOptions{
		Format:  src.Format,
		Streams: []ffgo.MediaType{ffgo.MediaTypeAudio},
	}
	if opts.ProbeBytes > 0 {
		fopts.ProbeSizeBytes = opts.ProbeBytes
	}
	if opts.AnalyzeDuration > 0 {
		fopts.AnalyzeDuration = opts.AnalyzeDuration
	}
	av := avOptions(src.Options)
	if hasScheme(src.URI) {
		av = networkAVOptions(av)
	}
	// FLAC with a SEEKTABLE can seek straight from the index instead of the
	// demuxer's binary search. That search is accurate but costs ~log2(size)
	// Range round trips — on a rate-limited CDN relay a drag took seconds.
	// The index can land seconds early, so Seek() then decodes past the target
	// (see discardBefore); fastSeek covers that.
	fastSeek := false
	if flags, ok := av["fflags"]; ok {
		fastSeek = strings.Contains(flags, "+fastseek")
	} else if src.Reader == nil && !src.IsLive() && flacHasSeekTable(ctx, src) {
		if av == nil {
			av = make(map[string]string)
		}
		av["fflags"] = "+fastseek"
		fastSeek = true
	}
	if len(av) > 0 {
		fopts.AVOptions = av
	}

	var (
		dec      *ffgo.Decoder
		seekable bool
		err      error
	)
	if src.IsStream() {
		_, seekable = src.Reader.(io.Seeker)
		dec, err = ffgo.NewDecoderFromReaderWithOptions(src.Reader, fopts)
	} else {
		path, perr := localPath(src.URI)
		if perr != nil {
			return nil, perr
		}
		seekable = true
		dec, err = ffgo.NewDecoderWithOptions(path, fopts)
	}
	if err != nil {
		return nil, fmt.Errorf("decode native: open %q: %w", src.Label(), err)
	}
	if ctx.Err() != nil {
		_ = dec.Close()
		return nil, ctx.Err()
	}
	if err := dec.OpenAudioDecoder(); err != nil {
		_ = dec.Close()
		return nil, fmt.Errorf("decode native: audio decoder: %w", err)
	}

	info := dec.AudioStream()
	if info == nil {
		_ = dec.Close()
		return nil, errors.New("decode native: no audio stream")
	}
	meta := decode.Meta{
		Codec:      info.CodecName,
		BitRate:    maxInt64(info.BitRate, dec.BitRate()),
		SampleRate: info.SampleRate,
		Channels:   info.Channels,
		Duration:   dec.Duration(),
		Seekable:   seekable,
		Live:       src.IsLive(),
	}
	if meta.Live {
		meta.Duration = 0
		meta.Seekable = false
	}

	frames := opts.ChunkFrames
	if frames <= 0 {
		frames = pcm.ChunkFrames
	}
	channels := meta.Channels
	if channels <= 0 {
		channels = pcm.DefaultChannels
	}
	return &stream{
		dec:      dec,
		meta:     meta,
		log:      opts.Log,
		fastSeek: fastSeek,
		sample:   meta.SampleRate,
		chans:    meta.Channels,
		chunk:    pcm.Chunk{Data: make([]float32, frames*channels), Frames: 0, Format: pcm.Format{SampleRate: pcm.DefaultSampleRate, Channels: channels}},
	}, nil
}

// stream decodes one media item into canonical PCM.
type stream struct {
	dec  *ffgo.Decoder
	meta decode.Meta
	log  func(string)

	// fastSeek is set when the container has a seek index (a FLAC SEEKTABLE)
	// and AVFMT_FLAG_FAST_SEEK was enabled. Seeks then land on the nearest
	// index point, so Seek decodes forward to the requested time.
	fastSeek bool
	// pending is the first frame decoded while discarding after a fast seek.
	// It belongs to the decoder and is consumed before the next DecodeAudio.
	pending ffgo.Frame
	hasPend bool
	sample  int // source sample rate, refreshed when the decoder changes it
	chans   int // source channel count

	resample   *ffgo.Resampler
	resampleTo ffgo.AudioFormat
	resampleFrom

	chunk  pcm.Chunk // returned buffer, grown on demand
	closed bool
	mu     sync.Mutex
}

// maxCorruptFrames bounds how many consecutive invalid frames a source may
// produce before the decoder is declared broken. One or two corrupt frames are
// common at the very end of CDN-hosted FLAC (the file ends with a truncated
// frame) and a seek can land on a partial one; endless invalid data is not.
const maxCorruptFrames = 16

type resampleFrom struct {
	rate, channels int
	format         ffgo.SampleFormat
}

func (s *stream) Format() pcm.Format { return s.chunk.Format }
func (s *stream) Meta() decode.Meta  { return s.meta }

// Read returns the next decoded chunk. The buffer is owned by the stream and
// stays valid until the following Read.
func (s *stream) Read() (pcm.Chunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return pcm.Chunk{}, decode.ErrNotOpen
	}

	corrupt := 0
	for {
		var frame ffgo.Frame
		if s.hasPend {
			frame, s.hasPend = s.pending, false
			s.pending = ffgo.Frame{}
		} else {
			next, err := s.dec.DecodeAudio()
			if err != nil {
				if errors.Is(err, io.EOF) || ffgo.IsEOF(err) {
					return pcm.Chunk{}, io.EOF
				}
				// A decoder-level invalid frame is not a dead source. QQ's FLAC
				// files routinely end with a truncated frame (the whole track
				// decodes, then the tail fails), and a seek can land on a partial
				// frame; FLAC frames are independent, so dropping one and reading
				// on resynchronises. Treating it as fatal ended the track right
				// before EOF — which also meant "ended" never fired and the next
				// song never started.
				if avutil.IsInvalidData(err) && corrupt < maxCorruptFrames {
					corrupt++
					if s.log != nil {
						s.log(fmt.Sprintf("decode native: skipping corrupt frame (%v)", err))
					}
					continue
				}
				return pcm.Chunk{}, err
			}
			frame = next
		}
		if frame.IsNil() {
			return pcm.Chunk{}, io.EOF
		}
		// The decoded frame belongs to the decoder and must not be freed.
		n, err := s.push(ffgo.WrapFrame(frame, ffgo.MediaTypeAudio))
		if err != nil {
			return pcm.Chunk{}, err
		}
		if n == 0 {
			// Resampler swallowed the frame (needs more input).
			continue
		}
		s.chunk.Frames = n
		return s.chunk, nil
	}
}

// push resamples one decoded frame into the chunk buffer and returns frames.
func (s *stream) push(w *ffgo.FrameWrapper) (int, error) {
	rate, channels, err := s.sourceFormat(w)
	if err != nil {
		return 0, err
	}
	format := w.SampleFormat()

	// Keep the source channel layout here. FFmpeg cannot rematrix layouts it
	// does not know (7.1.4 Opus, i.e. QQ Music's "臻品全景声 7.1", reports 12
	// channels with no layout), so asking swresample for 12 -> 2 fails inside
	// swr_convert and the whole track is silent. The player folds channels to
	// whatever the output endpoint accepts instead.
	if err := s.ensureResampler(rate, channels, format, channels); err != nil {
		return 0, err
	}
	// Some containers (raw streams, live inputs) probe without parameters; the
	// first decoded frame then becomes the source of truth for Meta().
	if s.meta.SampleRate <= 0 {
		s.meta.SampleRate = rate
	}
	if s.meta.Channels <= 0 {
		s.meta.Channels = channels
	}

	// Opus mapping family 3 (QQ Music's 7.1.4 tier) decodes 12 planes but
	// leaves the frame layout unspecified. swr_convert_frame reconfigures
	// itself from the input frame and then fails on the unknown layout; give it
	// the default layout for this channel count first.
	w.SetChannelLayout(channels)
	raw := w.Raw()
	out, err := s.resample.Resample(raw)
	if err != nil {
		return 0, fmt.Errorf("resample: %w", err)
	}
	if out.IsNil() {
		return 0, nil
	}
	defer func() { _ = out.Free() }()

	ow := ffgo.WrapFrame(out, ffgo.MediaTypeAudio)
	frames := ow.NumSamples()
	if frames <= 0 {
		return 0, nil
	}
	data := pcm.BytesF32(ow.Data(0))
	need := frames * s.resampleTo.Channels
	if len(data) < need {
		need = len(data)
		frames = need / s.resampleTo.Channels
	}
	if frames <= 0 {
		return 0, nil
	}

	outCh := s.resampleTo.Channels
	s.chunk.Data = ensureF32Cap(s.chunk.Data, frames*outCh)
	copy(s.chunk.Data[:frames*outCh], data[:frames*outCh])
	s.chunk.Format = pcm.Format{SampleRate: pcm.DefaultSampleRate, Channels: outCh}
	return frames, nil
}

// sourceFormat resolves the parameters of one decoded frame. Frame fields
// describe the buffers that are about to be read, so they win; the probed
// container parameters are the fallback for frames that carry no rate.
func (s *stream) sourceFormat(w *ffgo.FrameWrapper) (rate, channels int, err error) {
	rate, channels = w.SampleRate(), w.Channels()
	if rate <= 0 {
		rate = s.sample
	}
	if channels <= 0 {
		channels = s.chans
	}
	if rate <= 0 || channels <= 0 {
		return 0, 0, fmt.Errorf("%w: rate=%d channels=%d", decode.ErrBadFormat, rate, channels)
	}
	return rate, channels, nil
}

func (s *stream) ensureResampler(rate, channels int, format ffgo.SampleFormat, dstCh int) error {
	from := resampleFrom{rate: rate, channels: channels, format: format}
	to := ffgo.AudioFormat{
		SampleRate:   pcm.DefaultSampleRate,
		Channels:     dstCh,
		SampleFormat: ffgo.SampleFormatFlt,
	}
	if s.resample != nil && from == s.resampleFrom && to.Channels == s.resampleTo.Channels && to.SampleRate == s.resampleTo.SampleRate {
		return nil
	}
	if s.resample != nil {
		_ = s.resample.Close()
		s.resample = nil
	}
	r, err := ffgo.NewResampler(from.audioFormat(), to)
	if err != nil {
		return err
	}
	s.resample = r
	s.resampleFrom = from
	s.resampleTo = to
	s.sample = rate
	s.chans = channels
	return nil
}

func (f resampleFrom) audioFormat() ffgo.AudioFormat {
	return ffgo.AudioFormat{
		SampleRate:   f.rate,
		Channels:     f.channels,
		SampleFormat: f.format,
	}
}

// Seek repositions the demuxer and drops the resampler delay.
func (s *stream) Seek(t time.Duration) (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, decode.ErrNotOpen
	}
	if !s.meta.Seekable {
		return 0, decode.ErrSeekUnsupported
	}
	if t < 0 {
		t = 0
	}
	if err := s.dec.Seek(t); err != nil {
		return 0, err
	}
	// Force a fresh resampler context so no pre-seek samples leak through.
	if s.resample != nil {
		_ = s.resample.Close()
		s.resample = nil
	}
	s.dec.FlushDecoder()
	s.pending, s.hasPend = ffgo.Frame{}, false
	if s.fastSeek {
		// A fast seek lands on the nearest index point, which can be seconds
		// before t. Decode past it so the audio matches the clock the host set.
		if err := s.discardBefore(t); err != nil {
			return 0, err
		}
	}
	return t, nil
}

// maxDiscardFrames bounds how far a fast seek may decode forward. FLAC
// SEEKTABLE spacing is a handful of seconds (≈4096 samples per frame); 512
// frames covers ~47 s at 44.1 kHz, far beyond any sane index gap.
const maxDiscardFrames = 512

// discardBefore decodes and drops frames whose PTS is before t, keeping the
// first frame at/after t for the next Read. FLAC frame PTS are absolute sample
// numbers in the stream's sample rate.
func (s *stream) discardBefore(t time.Duration) error {
	rate := s.sample
	if rate <= 0 {
		return nil
	}
	target := int64(t.Seconds() * float64(rate))
	for i := 0; i < maxDiscardFrames; i++ {
		frame, err := s.dec.DecodeAudio()
		if err != nil {
			if errors.Is(err, io.EOF) || ffgo.IsEOF(err) {
				return nil // seeked past the tail: let Read report EOF
			}
			// Same tolerance as Read: one corrupt frame must not fail a seek.
			if avutil.IsInvalidData(err) {
				continue
			}
			return err
		}
		if frame.IsNil() {
			return nil
		}
		w := ffgo.WrapFrame(frame, ffgo.MediaTypeAudio)
		pts := w.PTS()
		if pts == avutil.AV_NOPTS_VALUE {
			// No timetable to verify against: keep it rather than drop audio.
			s.pending, s.hasPend = frame, true
			return nil
		}
		if pts >= target {
			s.pending, s.hasPend = frame, true
			return nil
		}
	}
	return nil // index gap larger than the bound: play from where we are
}

// Close releases FFmpeg contexts.
func (s *stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.resample != nil {
		_ = s.resample.Close()
		s.resample = nil
	}
	return s.dec.Close()
}

// avOptions maps "avopt:<key>" source options onto avformat_open_input.
func avOptions(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string)
	for k, v := range in {
		if name, ok := strings.CutPrefix(k, "avopt:"); ok {
			out[name] = v
		}
	}
	if ua, ok := in["user_agent"]; ok {
		if _, exists := out["user_agent"]; !exists {
			out["user_agent"] = ua
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Network AVIO defaults. A seek on an HTTP source is not one read: the
// demuxer's index search issues Range requests at arbitrary offsets, so a
// single slow origin reply can surface in the middle of it as
// "Error reading HTTP response: Connection timed out" followed by
// "read_timestamp() failed in the middle" — and a demuxer whose seek failed
// has undefined state. Give the range read the same budget the sidecar gives
// its own upstream (15 s) instead of the old 5 s, and let FFmpeg reconnect
// instead of aborting playback when a connection is reset mid-stream.
const (
	networkReadTimeout = "15000000" // microseconds
	networkReconnect   = "5"        // seconds between reconnect attempts
	networkMaxRetries  = "5"        // attempts before the read fails for real
)

func networkAVOptions(in map[string]string) map[string]string {
	out := in
	if out == nil {
		out = make(map[string]string)
	}
	// Caller-supplied avopt:* values (including a shorter rw_timeout) win.
	defaults := map[string]string{
		"rw_timeout":                 networkReadTimeout,
		"reconnect":                  "1",
		"reconnect_streamed":         "1",
		"reconnect_on_network_error": "1",
		"reconnect_delay_max":        networkReconnect,
		"reconnect_max_retries":      networkMaxRetries,
	}
	for k, v := range defaults {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	return out
}

// flacProbeBytes is enough for the STREAMINFO/SEEKTABLE/VORBIS_COMMENT/PADDING
// blocks QQ and ffmpeg write (they all fit in the first 8 KiB).
const flacProbeBytes = 8 << 10

// flacHasSeekTable reports whether src is a native FLAC stream carrying a
// SEEKTABLE metadata block. Sniffing costs one small Range read on a URL and
// is best-effort: any failure means "no index", and the decoder keeps FFmpeg's
// (accurate but slower) binary seek.
func flacHasSeekTable(ctx context.Context, src decode.Source) bool {
	// Probe only local files and the host's own /stream/ relay. A third-party
	// plugin can hand us a one-shot URL, and an extra GET would consume it;
	// those sources just keep the demuxer's (correct) binary seek.
	if hasScheme(src.URI) && !strings.HasPrefix(src.URI, "file://") && !strings.Contains(src.URI, "/stream/") {
		return false
	}
	head, err := readHead(ctx, src, flacProbeBytes)
	if err != nil || len(head) < 8 {
		return false
	}
	if string(head[:4]) != "fLaC" {
		return false
	}
	for off := 4; off+4 <= len(head); {
		h := head[off]
		typ := h & 0x7f
		last := h&0x80 != 0
		length := int(head[off+1])<<16 | int(head[off+2])<<8 | int(head[off+3])
		if typ == 3 { // SEEKTABLE
			return true
		}
		if last {
			return false
		}
		off += 4 + length
	}
	return false
}

// readHead returns up to n leading bytes of the source. Local files are read
// directly; http(s) uses a bounded Range request so a server that ignores
// Range cannot make the probe download the whole track.
func readHead(ctx context.Context, src decode.Source, n int) ([]byte, error) {
	if !hasScheme(src.URI) || strings.HasPrefix(src.URI, "file://") {
		path, err := localPath(src.URI)
		if err != nil {
			return nil, err
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		buf := make([]byte, n)
		m, _ := io.ReadFull(f, buf) // short reads are fine
		return buf[:m], nil
	}
	if !strings.HasPrefix(src.URI, "http://") && !strings.HasPrefix(src.URI, "https://") {
		return nil, errors.New("decode native: unsupported scheme for header probe")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))
	if ua := src.Options["user_agent"]; ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf := make([]byte, n)
	m, _ := io.ReadFull(io.LimitReader(resp.Body, int64(n)), buf)
	return buf[:m], nil
}

// localPath converts file:// URIs into plain paths and validates local files so
// a missing track fails fast with a clear error instead of an FFmpeg one.
func localPath(uri string) (string, error) {
	if uri == "" {
		return "", errors.New("decode native: empty source URI")
	}
	path := uri
	if rest, ok := strings.CutPrefix(uri, "file://"); ok {
		path = rest
		if len(path) > 1 && path[0] == '/' && path[2] == ':' { // file:///C:/x
			path = path[1:]
		}
	}
	if hasScheme(uri) {
		return path, nil
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("decode native: %w", err)
	}
	return path, nil
}

func hasScheme(uri string) bool {
	i := strings.Index(uri, "://")
	if i <= 0 {
		return false
	}
	for _, r := range uri[:i] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

func ensureF32Cap(b []float32, n int) []float32 {
	if cap(b) >= n {
		return b[:n]
	}
	return make([]float32, n)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
