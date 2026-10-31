//go:build windows && (amd64 || arm64)

package wmf

import (
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/runtime/win32"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/media/mediafoundation"
	comstorage "github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/com/structuredstorage"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/variant"

	"github.com/team-quaver/liino/decode"
	"github.com/team-quaver/liino/pcm"
)

// stream is one Source Reader session. The engine owns exactly one goroutine
// for Read; Seek/Close may arrive from another, so the mutex serialises every
// COM call on the reader.
type stream struct {
	reader *mediafoundation.IMFSourceReader
	meta   decode.Meta

	outRate int // negotiated decoded rate, normally 48 kHz
	outCh   int

	scratch []float32
	chunk   pcm.Chunk

	mu     sync.Mutex
	closed bool
	eof    bool
}

func newStream(reader *mediafoundation.IMFSourceReader, meta decode.Meta, rate, ch int) *stream {
	if ch <= 0 {
		ch = pcm.DefaultChannels
	}
	return &stream{
		reader:  reader,
		meta:    meta,
		outRate: rate,
		outCh:   ch,
		chunk: pcm.Chunk{
			Data:   make([]float32, pcm.ChunkFrames*ch),
			Format: pcm.Format{SampleRate: pcm.DefaultSampleRate, Channels: ch},
		},
	}
}

func (s *stream) Format() pcm.Format { return s.chunk.Format }
func (s *stream) Meta() decode.Meta  { return s.meta }

func (s *stream) Read() (pcm.Chunk, error) {
	release, err := comInit()
	if err != nil {
		return pcm.Chunk{}, err
	}
	defer release()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return pcm.Chunk{}, decode.ErrNotOpen
	}
	if s.eof {
		return pcm.Chunk{}, io.EOF
	}

	for {
		var (
			actual uint32
			flags  uint32
			ts     int64
			sample *mediafoundation.IMFSample
		)
		if err := s.reader.ReadSample(audioStreamIndex, 0, &actual, &flags, &ts, &sample); err != nil {
			return pcm.Chunk{}, fmt.Errorf("wmf: read: %w", err)
		}
		if flags&uint32(mediafoundation.MF_SOURCE_READERF_ERROR) != 0 {
			if sample != nil {
				sample.Release()
			}
			return pcm.Chunk{}, errors.New("wmf: source reader reported an error")
		}
		if flags&uint32(mediafoundation.MF_SOURCE_READERF_ENDOFSTREAM) != 0 {
			// The flag can arrive together with the final sample. Keep the
			// sample and report EOF on the next call so no audio is dropped.
			s.eof = true
		}
		if sample == nil {
			if s.eof {
				return pcm.Chunk{}, io.EOF
			}
			// STREAMTICK / gap: no data, keep reading.
			continue
		}
		buf, n, err := copySampleF32(sample, s.scratch)
		sample.Release()
		if err != nil {
			return pcm.Chunk{}, err
		}
		s.scratch = buf
		if n == 0 {
			continue
		}
		frames := n / s.outCh
		if frames == 0 {
			continue
		}
		return s.emit(buf, frames), nil
	}
}

// copySampleF32 copies one contiguous sample buffer into reused Go memory. The
// caller must release the sample afterwards: the pointer is only valid while
// the buffer is locked.
func copySampleF32(sample *mediafoundation.IMFSample, buf []float32) ([]float32, int, error) {
	var mb *mediafoundation.IMFMediaBuffer
	if err := sample.ConvertToContiguousBuffer(&mb); err != nil {
		return buf, 0, fmt.Errorf("wmf: contiguous buffer: %w", err)
	}
	defer mb.Release()

	var (
		p              *byte
		maxLen, curLen uint32
	)
	if err := mb.Lock(&p, &maxLen, &curLen); err != nil {
		return buf, 0, fmt.Errorf("wmf: lock sample: %w", err)
	}
	defer func() { _ = mb.Unlock() }()

	n := int(curLen) / 4
	if n == 0 || p == nil {
		return buf, 0, nil
	}
	if cap(buf) < n {
		buf = make([]float32, n)
	} else {
		buf = buf[:n]
	}
	copy(buf, unsafe.Slice((*float32)(unsafe.Pointer(p)), n))
	return buf, n, nil
}

// emit folds the negotiated rate into the canonical 48 kHz. Media Foundation
// normally does that itself; the Go resampler is only the fallback for decoders
// whose converter refuses 48 kHz.
func (s *stream) emit(buf []float32, frames int) pcm.Chunk {
	ch := s.outCh
	if s.outRate == pcm.DefaultSampleRate {
		n := frames * ch
		s.chunk.Data = ensureCap(s.chunk.Data, n)
		copy(s.chunk.Data[:n], buf[:n])
		s.chunk.Frames = frames
		s.chunk.Format = pcm.Format{SampleRate: pcm.DefaultSampleRate, Channels: ch}
		return s.chunk
	}
	want := (frames*pcm.DefaultSampleRate + s.outRate/2) / s.outRate
	s.chunk.Data = ensureCap(s.chunk.Data, want*ch)
	pcm.ResamplePoly(s.chunk.Data[:want*ch], buf[:frames*ch], s.outRate, pcm.DefaultSampleRate, ch, 32)
	s.chunk.Frames = want
	s.chunk.Format = pcm.Format{SampleRate: pcm.DefaultSampleRate, Channels: ch}
	return s.chunk
}

func (s *stream) Seek(t time.Duration) (time.Duration, error) {
	release, err := comInit()
	if err != nil {
		return 0, err
	}
	defer release()

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
	// kCMTimeFormat / GUID_NULL means "100-nanosecond units", which is what
	// PROPVARIANT VT_I8 carries below.
	pv := propVariantInt64(int64(t / (100 * time.Nanosecond)))
	if err := s.reader.SetCurrentPosition(nil, (*comstorage.PROPVARIANT)(unsafe.Pointer(&pv))); err != nil {
		return 0, fmt.Errorf("wmf: seek: %w", err)
	}
	_ = s.reader.Flush(audioStreamIndex)
	s.eof = false
	return t, nil
}

func (s *stream) Close() error {
	release, _ := comInit()
	if release != nil {
		defer release()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.reader != nil {
		s.reader.Release()
		s.reader = nil
	}
	return nil
}

// propVariant mirrors the 24-byte ProPVARIANT layout: vt + three reserved
// WORDs, then the 16-byte union. The generated binding exposes the union as
// raw storage without the vt header, so a typed cast keeps offsets honest.
type propVariant struct {
	vt        uint16
	reserved1 uint16
	reserved2 uint16
	reserved3 uint16
	data      [2]uint64
}

func propVariantInt64(v int64) propVariant {
	p := propVariant{vt: uint16(variant.VT_I8)}
	p.data[0] = uint64(v)
	return p
}

// presentationDuration reads MF_PD_DURATION (100-ns VT_UI8) from the audio
// stream. A missing attribute means live/unknown, i.e. duration 0.
func presentationDuration(reader *mediafoundation.IMFSourceReader) time.Duration {
	var pv comstorage.PROPVARIANT
	// Presentation-descriptor attributes live on the media source, not on the
	// audio stream; passing the stream index returns nothing.
	if err := reader.GetPresentationAttribute(mediaSourceIndex, &mediafoundation.MF_PD_DURATION, &pv); err != nil {
		return 0
	}
	v := (*propVariant)(unsafe.Pointer(&pv))
	if v.vt != uint16(variant.VT_UI8) || v.data[0] == 0 {
		return 0
	}
	if v.data[0] > uint64(math.MaxInt64/100) {
		return 0
	}
	return time.Duration(v.data[0]*100) * time.Nanosecond
}

func outputMediaType(rate, ch, channelMask uint32) (*mediafoundation.IMFMediaType, error) {
	var out *mediafoundation.IMFMediaType
	if err := mediafoundation.MFCreateMediaType(&out); err != nil {
		return nil, err
	}
	fail := func(err error) (*mediafoundation.IMFMediaType, error) {
		out.Release()
		return nil, err
	}
	if err := out.SetGUID(&mediafoundation.MF_MT_MAJOR_TYPE, &mediafoundation.MFMediaType_Audio); err != nil {
		return fail(err)
	}
	if err := out.SetGUID(&mediafoundation.MF_MT_SUBTYPE, &mediafoundation.MFAudioFormat_Float); err != nil {
		return fail(err)
	}
	if err := out.SetUINT32(&mediafoundation.MF_MT_AUDIO_NUM_CHANNELS, ch); err != nil {
		return fail(err)
	}
	// Carry the source layout across the decoder conversion. Without it the
	// audio converter cannot map 5.1/7.1 into the requested channel count and
	// SetCurrentMediaType fails on surround tracks.
	if channelMask != 0 {
		if err := out.SetUINT32(&mediafoundation.MF_MT_AUDIO_CHANNEL_MASK, channelMask); err != nil {
			return fail(err)
		}
	}
	if err := out.SetUINT32(&mediafoundation.MF_MT_AUDIO_SAMPLES_PER_SECOND, rate); err != nil {
		return fail(err)
	}
	if err := out.SetUINT32(&mediafoundation.MF_MT_AUDIO_BITS_PER_SAMPLE, 32); err != nil {
		return fail(err)
	}
	if err := out.SetUINT32(&mediafoundation.MF_MT_AUDIO_BLOCK_ALIGNMENT, ch*4); err != nil {
		return fail(err)
	}
	if err := out.SetUINT32(&mediafoundation.MF_MT_AUDIO_AVG_BYTES_PER_SECOND, rate*ch*4); err != nil {
		return fail(err)
	}
	return out, nil
}

func codecName(sub win32.GUID) string {
	switch sub {
	case mediafoundation.MFAudioFormat_MP3:
		return "mp3"
	case mediafoundation.MFAudioFormat_AAC:
		return "aac"
	case mediafoundation.MFAudioFormat_ALAC:
		return "alac"
	case mediafoundation.MFAudioFormat_FLAC:
		return "flac"
	case mediafoundation.MFAudioFormat_WMAudioV8,
		mediafoundation.MFAudioFormat_WMAudioV9:
		return "wma"
	case mediafoundation.MFAudioFormat_WMAudio_Lossless:
		return "wma lossless"
	case mediafoundation.MFAudioFormat_Dolby_AC3:
		return "ac3"
	case mediafoundation.MFAudioFormat_Dolby_DDPlus:
		return "eac3"
	case mediafoundation.MFAudioFormat_Opus:
		return "opus"
	case mediafoundation.MFAudioFormat_Vorbis:
		return "vorbis"
	case mediafoundation.MFAudioFormat_PCM:
		return "pcm"
	case mediafoundation.MFAudioFormat_Float:
		return "pcm float"
	default:
		return fmt.Sprintf("mf:0x%08x", sub.Data1)
	}
}

func ensureCap(b []float32, n int) []float32 {
	if cap(b) >= n {
		return b[:n]
	}
	return make([]float32, n)
}
