//go:build !android && !ios && (amd64 || arm64)

package native

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/team-quaver/ffgo"

	"github.com/team-quaver/liino/decode"
	"github.com/team-quaver/liino/pcm"
)

// Probe checks that the FFmpeg shared libraries resolve in this process.
func (backend) Probe() (bool, string) {
	if err := ffgo.Init(); err != nil {
		return false, "ffgo: " + err.Error()
	}
	au, ac, af := ffgo.Version()
	return true, fmt.Sprintf("ffmpeg avutil=%d avcodec=%d avformat=%d", au, ac, af)
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
	// Network sources go through FFmpeg's default AVIO stack, which has no read
	// deadline by default: a stalled CDN connection parks the decoder goroutine
	// forever and Close() waits on it. 5 s is long enough for a healthy range
	// read and short enough to turn a dead peer into a normal playback error.
	if hasScheme(src.URI) {
		if av == nil {
			av = make(map[string]string)
		}
		if _, ok := av["rw_timeout"]; !ok {
			av["rw_timeout"] = "5000000"
		}
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
		dec:    dec,
		meta:   meta,
		sample: meta.SampleRate,
		chans:  meta.Channels,
		chunk:  pcm.Chunk{Data: make([]float32, frames*channels), Frames: 0, Format: pcm.Format{SampleRate: pcm.DefaultSampleRate, Channels: channels}},
	}, nil
}

// stream decodes one media item into canonical PCM.
type stream struct {
	dec  *ffgo.Decoder
	meta decode.Meta

	sample int // source sample rate, refreshed when the decoder changes it
	chans  int // source channel count

	resample   *ffgo.Resampler
	resampleTo ffgo.AudioFormat
	resampleFrom

	chunk  pcm.Chunk // returned buffer, grown on demand
	closed bool
	mu     sync.Mutex
}

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

	for {
		frame, err := s.dec.DecodeAudio()
		if err != nil {
			if errors.Is(err, io.EOF) || ffgo.IsEOF(err) {
				return pcm.Chunk{}, io.EOF
			}
			return pcm.Chunk{}, err
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
	return t, nil
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
