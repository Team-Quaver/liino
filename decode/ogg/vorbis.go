package ogg

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jfreymuth/oggvorbis"

	"github.com/team-quaver/liino/decode"
	"github.com/team-quaver/liino/pcm"
)

// vorbisStream wraps oggvorbis.Reader and normalises its float32 output to the
// canonical rate. oggvorbis decodes at the stream's own rate, so 44.1 kHz
// files need the same polyphase resampler avf uses; 48 kHz files are copied
// straight through.
type vorbisStream struct {
	r     *oggvorbis.Reader
	rate  int
	ch    int
	meta  decode.Meta
	chunk pcm.Chunk

	scratch  []float32 // one read at the source rate
	inFrames int       // source frames that fill about one chunk

	eof     bool
	closeFn func() error

	mu     sync.Mutex
	closed bool
}

func newVorbis(src io.Reader, seekable bool, closeFn func() error) (decode.Stream, error) {
	r, err := oggvorbis.NewReader(src)
	if err != nil {
		return nil, fmt.Errorf("ogg: vorbis: %w", err)
	}
	rate, ch := r.SampleRate(), r.Channels()
	if rate <= 0 || ch <= 0 {
		return nil, decode.ErrBadFormat
	}

	meta := decode.Meta{Codec: codecVorbis, SampleRate: rate, Channels: ch}
	if length := r.Length(); seekable && length > 0 {
		meta.Seekable = true
		meta.Duration = samplesDuration(length, rate)
	}

	inFrames := pcm.ChunkFrames
	if rate != pcm.DefaultSampleRate {
		inFrames = (pcm.ChunkFrames*rate + pcm.DefaultSampleRate - 1) / pcm.DefaultSampleRate
	}
	return &vorbisStream{
		r:        r,
		rate:     rate,
		ch:       ch,
		meta:     meta,
		scratch:  make([]float32, inFrames*ch),
		inFrames: inFrames,
		chunk: pcm.Chunk{
			Data:   make([]float32, pcm.ChunkFrames*ch),
			Format: pcm.Format{SampleRate: pcm.DefaultSampleRate, Channels: ch},
		},
		closeFn: closeFn,
	}, nil
}

func (s *vorbisStream) Format() pcm.Format { return s.chunk.Format }

func (s *vorbisStream) Meta() decode.Meta { return s.meta }

func (s *vorbisStream) Read() (pcm.Chunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return pcm.Chunk{}, decode.ErrNotOpen
	}
	if s.eof {
		return pcm.Chunk{}, io.EOF
	}

	for {
		n, err := s.r.Read(s.scratch)
		frames := n / s.ch
		if frames > 0 {
			if errors.Is(err, io.EOF) {
				s.eof = true
			} else if err != nil {
				return pcm.Chunk{}, fmt.Errorf("ogg: vorbis: %w", err)
			}
			return s.emit(frames), nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.eof = true
			}
			return pcm.Chunk{}, err
		}
	}
}

func (s *vorbisStream) emit(frames int) pcm.Chunk {
	in := s.scratch[:frames*s.ch]
	if s.rate == pcm.DefaultSampleRate {
		s.chunk.Data = ensureCap(s.chunk.Data, len(in))
		copy(s.chunk.Data, in)
		s.chunk.Frames = frames
		return s.chunk
	}
	want := (frames*pcm.DefaultSampleRate + s.rate/2) / s.rate
	s.chunk.Data = ensureCap(s.chunk.Data, want*s.ch)
	pcm.ResamplePoly(s.chunk.Data, in, s.rate, pcm.DefaultSampleRate, s.ch, 32)
	s.chunk.Frames = want
	return s.chunk
}

func (s *vorbisStream) Seek(t time.Duration) (time.Duration, error) {
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
	if s.meta.Duration > 0 && t >= s.meta.Duration {
		t = s.meta.Duration - time.Millisecond
		if t < 0 {
			t = 0
		}
	}
	pos := int64(t) * int64(s.rate) / int64(time.Second)
	if err := s.r.SetPosition(pos); err != nil {
		return 0, fmt.Errorf("ogg: vorbis seek: %w", err)
	}
	s.eof = false
	return samplesDuration(pos, s.rate), nil
}

func (s *vorbisStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.closeFn()
}

// samplesDuration converts a sample count to a duration without overflowing
// time.Duration for very long streams.
func samplesDuration(samples int64, rate int) time.Duration {
	if samples <= 0 || rate <= 0 {
		return 0
	}
	return time.Duration(float64(samples) / float64(rate) * float64(time.Second))
}

// durationSamples is the inverse of samplesDuration, used to turn a seek
// target into a sample position at the decoder's rate.
func durationSamples(d time.Duration, rate int) int64 {
	if d <= 0 || rate <= 0 {
		return 0
	}
	return int64(float64(d) / float64(time.Second) * float64(rate))
}
