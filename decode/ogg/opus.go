package ogg

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	gopus "github.com/thesyncim/gopus"
	gogg "github.com/thesyncim/gopus/container/ogg"

	"github.com/team-quaver/liino/decode"
	"github.com/team-quaver/liino/pcm"
)

// Opus always decodes at 48 kHz, which is already Liino's canonical rate.
// Packets can carry up to 120 ms, so Read buffers decoded frames until it has
// a full chunk.
const (
	opusRate        = pcm.DefaultSampleRate
	opusMaxPacket   = 61_440 // RFC 7845 section 6 expected maximum
	opusMaxSamples  = 5_760  // 120 ms at 48 kHz
	opusSeekPreRoll = 3_840  // 80 ms, libopus' recommended pre-roll
)

// opusDecoder is the common surface of gopus's mono/stereo and multistream
// decoders.
type opusDecoder interface {
	Decode(packet []byte, out []float32) (int, error)
	Reset()
	SetGain(gainQ8 int) error
}

type opusStream struct {
	r        *gogg.Reader
	dec      opusDecoder
	ch       int
	meta     decode.Meta
	preSkip  int64
	total    int64 // frames after pre-skip; -1 when the source length is unknown
	remain   int64 // frames still to emit (including pending); -1 when unknown
	skip     int64 // pre-skip frames still to drop at the start
	seekTo   int64 // absolute granule position to start output at; 0 = none
	pkt      []byte
	scratch  []float32
	pending  []float32
	pendingN int
	eof      bool
	chunk    pcm.Chunk

	closeFn func() error
	mu      sync.Mutex
	closed  bool
}

func newOpus(src io.Reader, rs io.ReadSeeker, serial uint32, closeFn func() error) (decode.Stream, error) {
	var last int64 = -1
	if rs != nil {
		if g, ok, err := lastGranule(rs, serial); err == nil && ok {
			last = int64(g)
		}
		if _, err := rs.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("ogg: opus rewind: %w", err)
		}
	}

	r, err := gogg.NewReader(src)
	if err != nil {
		return nil, fmt.Errorf("ogg: opus: %w", err)
	}
	head := r.Header
	ch := int(head.Channels)
	if ch <= 0 {
		return nil, decode.ErrBadFormat
	}
	dec, err := newOpusDecoder(head)
	if err != nil {
		return nil, err
	}
	if err := dec.SetGain(int(head.OutputGain)); err != nil {
		return nil, fmt.Errorf("ogg: opus gain: %w", err)
	}

	preSkip := int64(head.PreSkip)
	total := int64(-1)
	if last >= 0 {
		total = last - preSkip
		if total < 0 {
			total = 0
		}
	}
	meta := decode.Meta{
		Codec:      codecOpus,
		SampleRate: opusRate,
		Channels:   ch,
		Seekable:   rs != nil,
	}
	if total >= 0 {
		meta.Duration = samplesDuration(total, opusRate)
	}

	return &opusStream{
		r:       r,
		dec:     dec,
		ch:      ch,
		meta:    meta,
		preSkip: preSkip,
		total:   total,
		remain:  total,
		skip:    preSkip,
		pkt:     make([]byte, opusMaxPacket),
		scratch: make([]float32, opusMaxSamples*ch),
		chunk: pcm.Chunk{
			Data:   make([]float32, pcm.ChunkFrames*ch),
			Format: pcm.Format{SampleRate: opusRate, Channels: ch},
		},
		closeFn: closeFn,
	}, nil
}

// newOpusDecoder handles family 0 (mono/stereo), plus families 1, 2 and 255,
// which all carry an explicit channel map for 5.1, 7.1, 7.1.4 and discrete
// layouts. Family 3 stores a projection matrix instead and falls back to ffgo.
func newOpusDecoder(head *gogg.OpusHead) (opusDecoder, error) {
	ch := int(head.Channels)
	switch head.MappingFamily {
	case 0:
		if ch < 1 || ch > 2 {
			return nil, fmt.Errorf("%w: opus family 0 with %d channels", decode.ErrUnsupported, ch)
		}
		return gopus.NewDecoder(gopus.DefaultDecoderConfig(opusRate, ch))
	case 1, 2, 255:
		mapping := head.ChannelMapping
		if head.StreamCount == 0 || len(mapping) != ch {
			return nil, fmt.Errorf("%w: bad opus channel mapping", decode.ErrUnsupported)
		}
		return gopus.NewMultistreamDecoder(opusRate, ch, int(head.StreamCount), int(head.CoupledCount), mapping)
	default:
		return nil, fmt.Errorf("%w: opus mapping family %d", decode.ErrUnsupported, head.MappingFamily)
	}
}

func (s *opusStream) Format() pcm.Format { return s.chunk.Format }

func (s *opusStream) Meta() decode.Meta { return s.meta }

func (s *opusStream) Read() (pcm.Chunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return pcm.Chunk{}, decode.ErrNotOpen
	}
	for s.pendingN < pcm.ChunkFrames && !s.eof {
		if err := s.decodeNext(); err != nil {
			return pcm.Chunk{}, err
		}
	}
	if s.pendingN == 0 {
		s.eof = true
		return pcm.Chunk{}, io.EOF
	}
	return s.emit(), nil
}

func (s *opusStream) decodeNext() error {
	n, granule, err := s.r.ReadPacketInto(s.pkt)
	if err != nil {
		if errors.Is(err, io.EOF) {
			s.eof = true
			return nil
		}
		return fmt.Errorf("ogg: opus demux: %w", err)
	}
	if n == 0 {
		return nil
	}
	frames, err := s.dec.Decode(s.pkt[:n], s.scratch)
	if err != nil {
		return fmt.Errorf("ogg: opus decode: %w", err)
	}
	if frames <= 0 {
		return nil
	}
	s.appendPacket(s.scratch[:frames*s.ch], frames, granule)
	return nil
}

// appendPacket applies the three trims an Ogg Opus stream needs: the encoder
// pre-skip at the start, the exact target after a seek (SeekGranule starts on a
// packet boundary), and end padding beyond the last granule position.
func (s *opusStream) appendPacket(in []float32, frames int, end uint64) {
	if s.skip > 0 {
		drop := s.skip
		if drop > int64(frames) {
			drop = int64(frames)
		}
		in = in[int(drop)*s.ch:]
		frames -= int(drop)
		s.skip -= drop
		if frames == 0 {
			return
		}
	}
	if s.seekTo > 0 {
		start := int64(end) - int64(frames)
		if start < s.seekTo {
			drop := s.seekTo - start
			if drop >= int64(frames) {
				return
			}
			in = in[int(drop)*s.ch:]
			frames -= int(drop)
		}
		s.seekTo = 0
	}
	if s.remain >= 0 {
		if s.remain == 0 {
			s.eof = true
			return
		}
		if int64(frames) > s.remain {
			frames = int(s.remain)
		}
		s.remain -= int64(frames)
		if s.remain == 0 {
			s.eof = true
		}
	}
	s.pending = append(s.pending, in[:frames*s.ch]...)
	s.pendingN += frames
}

func (s *opusStream) emit() pcm.Chunk {
	frames := s.pendingN
	if frames > pcm.ChunkFrames {
		frames = pcm.ChunkFrames
	}
	n := frames * s.ch
	s.chunk.Data = ensureCap(s.chunk.Data, n)
	copy(s.chunk.Data, s.pending[:n])
	rest := copy(s.pending, s.pending[n:])
	s.pending = s.pending[:rest]
	s.pendingN -= frames
	s.chunk.Frames = frames
	return s.chunk
}

func (s *opusStream) Seek(t time.Duration) (time.Duration, error) {
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
	target := durationSamples(t, opusRate)
	if s.total >= 0 && target >= s.total {
		target = s.total - int64(opusRate/1000)
		if target < 0 {
			target = 0
		}
	}
	if err := s.rewind(target); err != nil {
		return 0, err
	}
	return samplesDuration(target, opusRate), nil
}

// rewind seeks the demuxer to a packet a little before target and lets the
// decoder discard up to the exact sample. Starting early is what gives the
// codec state time to converge again after Reset.
func (s *opusStream) rewind(target int64) error {
	granule := uint64(target) + uint64(s.preSkip)
	start := granule
	if start > opusSeekPreRoll {
		start -= opusSeekPreRoll
	} else {
		start = 0
	}
	if err := s.r.SeekGranule(start); err != nil {
		return fmt.Errorf("ogg: opus seek: %w", err)
	}
	s.dec.Reset()
	s.pending = s.pending[:0]
	s.pendingN = 0
	s.eof = false
	s.skip = 0
	s.seekTo = int64(granule)
	if s.total >= 0 {
		s.remain = s.total - target
	} else {
		s.remain = -1
	}
	return nil
}

func (s *opusStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.closeFn()
}

// lastGranule finds the granule position of the final page, which is the
// stream's length plus pre-skip. Ogg pages are at most ~65 KiB, so the last
// page is always inside a 64 KiB tail.
func lastGranule(rs io.ReadSeeker, serial uint32) (uint64, bool, error) {
	end, err := rs.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, false, err
	}
	const tail = 64 * 1024
	size := end
	if size > tail {
		size = tail
	}
	if size < 27 {
		return 0, false, nil
	}
	buf := make([]byte, size)
	if _, err := rs.Seek(end-size, io.SeekStart); err != nil {
		return 0, false, err
	}
	if _, err := io.ReadFull(rs, buf); err != nil {
		return 0, false, err
	}
	for i := len(buf) - 27; i >= 0; i-- {
		if buf[i] != 'O' || buf[i+1] != 'g' || buf[i+2] != 'g' || buf[i+3] != 'S' || buf[i+4] != 0 {
			continue
		}
		if binary.LittleEndian.Uint32(buf[i+14:]) != serial {
			continue
		}
		if g := binary.LittleEndian.Uint64(buf[i+6:]); g != ^uint64(0) {
			return g, true, nil
		}
	}
	return 0, false, nil
}
