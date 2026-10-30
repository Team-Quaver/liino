// Package pcm holds the canonical PCM layout used by the Liino pipeline:
// interleaved float32, 48 kHz, stereo.
package pcm

import (
	"encoding/binary"
	"math"
)

// Defaults for the fixed pipeline format. 2400 frames = 50 ms at 48 kHz,
// matching Quaver's PCM compatibility floor (ui/electron/audio/engine.ts).
const (
	DefaultSampleRate = 48000
	DefaultChannels   = 2
	ChunkFrames       = 2400
)

// Format describes the interleaved float32 PCM used end to end.
type Format struct {
	SampleRate int
	Channels   int
}

func DefaultFormat() Format {
	return Format{SampleRate: DefaultSampleRate, Channels: DefaultChannels}
}

// Bytes returns the size of one interleaved frame.
func (f Format) Bytes() int { return f.Channels * 4 }

// FrameBytes returns the byte size of n frames.
func (f Format) FrameBytes(n int) int { return n * f.Bytes() }

// Duration returns the duration of n frames at the format rate.
func (f Format) Duration(n int) float64 {
	if f.SampleRate <= 0 {
		return 0
	}
	return float64(n) / float64(f.SampleRate)
}

// Chunk holds one interleaved float32 buffer plus its frame count.
type Chunk struct {
	Data   []float32 // interleaved, length == Frames*Channels
	Frames int
	Format Format
}

func NewChunk(frames int) Chunk {
	return Chunk{Data: make([]float32, frames*DefaultChannels), Frames: frames, Format: DefaultFormat()}
}

// Silence zeroes the chunk data.
func (c *Chunk) Silence() { clear(c.Data) }

// Clone returns a deep copy of the chunk.
func (c Chunk) Clone() Chunk {
	out := Chunk{Frames: c.Frames, Format: c.Format}
	if c.Data != nil {
		out.Data = append([]float32(nil), c.Data...)
	}
	return out
}

// S16ToF32 converts interleaved signed 16-bit PCM to float32 in place into dst.
// dst must have room for samples frames*channels.
func S16ToF32(dst []float32, src []byte, frames, channels int) {
	n := frames * channels
	for i := 0; i < n; i++ {
		dst[i] = float32(int16(binary.LittleEndian.Uint16(src[i*2:]))) / 32768
	}
}

// F32ToS16 converts interleaved float32 to signed 16-bit PCM.
func F32ToS16(dst []byte, src []float32, frames, channels int) {
	n := frames * channels
	for i := 0; i < n; i++ {
		v := src[i]
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(int16(v*32767)))
	}
}

// DownmixToStereo converts a multichannel interleaved float32 buffer to
// stereo by dropping extra channels; mono is duplicated to both outputs.
func DownmixToStereo(out, in []float32, frames, channels int) {
	switch channels {
	case 1:
		for i := 0; i < frames; i++ {
			v := in[i]
			out[i*2] = v
			out[i*2+1] = v
		}
	default:
		for i := 0; i < frames; i++ {
			out[i*2] = in[i*channels]
			out[i*2+1] = in[i*channels+1]
		}
	}
}

// ResamplePoly converts interleaved float32 between sample rates using a
// windowed-sinc polyphase FIR. Pure-Go fallback for backends that cannot
// resample; the two built-in decode backends already normalize to 48 kHz,
// so this is only used for test and edge paths.
func ResamplePoly(out []float32, in []float32, inRate, outRate, channels int, quality int) {
	if quality < 4 {
		quality = 16
	}
	if quality > 64 {
		quality = 64
	}
	inFrames := len(in) / channels
	outFrames := len(out) / channels
	if inFrames == 0 || channels == 0 || outFrames == 0 {
		clear(out)
		return
	}
	ratio := float64(inRate) / float64(outRate)
	taps := quality
	for ch := 0; ch < channels; ch++ {
		for of := 0; of < outFrames; of++ {
			center := (float64(of)+0.5)*ratio - 0.5
			start := int(math.Floor(center - float64(taps)/2 + 1))
			var acc, wsum float64
			for t := 0; t < taps; t++ {
				idx := start + t
				if idx < 0 || idx >= inFrames {
					continue
				}
				pos := (float64(idx) + 0.5 - center) / ratio
				if pos == 0 {
					acc += float64(in[idx*channels+ch])
					wsum++
					continue
				}
				wp := pos / float64(taps/2)
				if wp < -1 || wp > 1 {
					continue
				}
				win := 0.42 - 0.5*math.Cos(2*math.Pi*pos/float64(taps)) + 0.08*math.Cos(4*math.Pi*pos/float64(taps))
				s := math.Sin(math.Pi*pos) / (math.Pi * pos) * win
				acc += float64(in[idx*channels+ch]) * s
				wsum += s
			}
			v := 0.0
			if wsum != 0 {
				v = acc / wsum
			}
			if v > 1 {
				v = 1
			} else if v < -1 {
				v = -1
			}
			out[of*channels+ch] = float32(v)
		}
	}
}

// Ring is a thread-safe fixed-size ring buffer of interleaved float32 frames.
type Ring struct {
	mu    chan struct{}
	data  []float32
	cap   int
	start int
	count int
	fmt   Format
}

// NewRing creates a ring buffer holding capacity frames.
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = ChunkFrames * 4
	}
	return &Ring{
		mu:   make(chan struct{}, 1),
		data: make([]float32, capacity*DefaultChannels),
		cap:  capacity,
		fmt:  DefaultFormat(),
	}
}

func (r *Ring) lock()   { r.mu <- struct{}{} }
func (r *Ring) unlock() { <-r.mu }

// Write appends frames from c; returns the number of frames actually written
// (0 when the ring is full).
func (r *Ring) Write(c Chunk) int {
	if r.cap == 0 || c.Frames <= 0 {
		return 0
	}
	r.lock()
	defer r.unlock()
	frames := c.Frames
	if frames > r.cap-r.count {
		frames = r.cap - r.count
	}
	channels := r.fmt.Channels
	head := (r.start + r.count) % r.cap
	for i := 0; i < frames; i++ {
		idx := (head + i) % r.cap
		copy(r.data[idx*channels:(idx+1)*channels], c.Data[i*channels:(i+1)*channels])
	}
	r.count += frames
	return frames
}

// Read copies up to frames frames into c.Data and returns the chunk plus
// whether any data was returned. The returned chunk shares the caller's
// backing buffer (resized as needed).
func (r *Ring) Read(frames int) (Chunk, bool) {
	r.lock()
	defer r.unlock()
	if frames > r.count {
		frames = r.count
	}
	channels := r.fmt.Channels
	c := Chunk{Frames: frames, Format: r.fmt}
	if cap(c.Data) < frames*channels {
		c.Data = make([]float32, frames*channels)
	}
	c.Data = c.Data[:frames*channels]
	for i := 0; i < frames; i++ {
		idx := (r.start + i) % r.cap
		copy(c.Data[i*channels:(i+1)*channels], r.data[idx*channels:(idx+1)*channels])
	}
	r.start = (r.start + frames) % r.cap
	r.count -= frames
	return c, frames > 0
}

// Available returns the number of frames currently buffered.
func (r *Ring) Available() int {
	r.lock()
	defer r.unlock()
	return r.count
}

// Capacity returns the ring capacity in frames.
func (r *Ring) Capacity() int { return r.cap }

// Reset empties the ring.
func (r *Ring) Reset() {
	r.lock()
	defer r.unlock()
	r.start = 0
	r.count = 0
}

// DownmixChannels folds an interleaved multichannel buffer into stereo with
// film-style coefficients. Channel counts it does not know about fall back to
// the first two channels.
func DownmixChannels(out, in []float32, frames, channels int) {
	switch channels {
	case 1:
		DownmixToStereo(out, in, frames, 1)
	case 2:
		if len(out) >= frames*2 {
			copy(out[:frames*2], in[:frames*2])
		}
	case 3: // L C R
		for i := 0; i < frames; i++ {
			c := in[i*3+1]
			out[i*2] = in[i*3] + 0.7*c
			out[i*2+1] = in[i*3+2] + 0.7*c
		}
	case 4: // L R Sl Sr
		for i := 0; i < frames; i++ {
			b := in[i*4:]
			out[i*2] = b[0] + 0.7*b[2]
			out[i*2+1] = b[1] + 0.7*b[3]
		}
	case 5: // L R C Sl Sr
		for i := 0; i < frames; i++ {
			b := in[i*5:]
			out[i*2] = b[0] + 0.7*b[2] + 0.7*b[3]
			out[i*2+1] = b[1] + 0.7*b[2] + 0.7*b[4]
		}
	case 6: // 5.1: L R C LFE Ls Rs
		for i := 0; i < frames; i++ {
			b := in[i*6:]
			lfe := 0.5 * b[3]
			out[i*2] = b[0] + 0.7*b[2] + 0.7*b[4] + lfe
			out[i*2+1] = b[1] + 0.7*b[2] + 0.7*b[5] + lfe
		}
	case 8: // 7.1: L R C LFE Ls Rs Lb Rb
		for i := 0; i < frames; i++ {
			b := in[i*8:]
			lfe := 0.5 * b[3]
			out[i*2] = b[0] + 0.7*b[2] + 0.7*b[4] + 0.7*b[6] + lfe
			out[i*2+1] = b[1] + 0.7*b[2] + 0.7*b[5] + 0.7*b[7] + lfe
		}
	default:
		DownmixToStereo(out, in, frames, channels)
	}
}
