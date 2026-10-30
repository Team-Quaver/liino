package pcm

import (
	"errors"
	"sync"
)

// Errors returned by a Pipe.
var (
	// ErrPipeClosed is returned by Write once the pipe has been closed.
	ErrPipeClosed = errors.New("pcm: pipe closed")
	// ErrPipeInterrupted is returned when a blocked Write is cancelled by the
	// owner (seek, track change, shutdown) before the pipe was drained.
	ErrPipeInterrupted = errors.New("pcm: pipe interrupted")
	// ErrPipeStale is returned when a producer from an older generation tries
	// to write after the owner swapped to a new one.
	ErrPipeStale = errors.New("pcm: pipe belongs to a newer generation")
)

// Pipe is a bounded ring of interleaved float32 frames that decouples the
// decode goroutine (producer, may block) from the platform audio callback
// (consumer, must never block).
//
// The producer calls Write and blocks while the pipe is full; the audio
// callback calls Read, which is non-blocking and returns whatever is buffered.
// Reset and Close broadcast so a blocked producer wakes up immediately on a
// seek, a track change, or a shutdown.
type Pipe struct {
	mu       sync.Mutex
	notEmpty *sync.Cond
	notFull  *sync.Cond

	buf      []float32 // capacity*Channels, ring storage
	capacity int       // frames
	start    int       // index of the oldest buffered frame
	count    int       // buffered frames

	format      Format
	token       uint64 // active producer generation
	closed      bool
	interrupted bool
	dropped     uint64 // frames discarded by Reset, for timeline bookkeeping
}

// NewPipe creates a pipe holding capacity frames in the given format.
func NewPipe(capacity int, format Format) *Pipe {
	if capacity <= 0 {
		capacity = ChunkFrames * 4
	}
	if format.Channels <= 0 {
		format.Channels = DefaultChannels
	}
	if format.SampleRate <= 0 {
		format.SampleRate = DefaultSampleRate
	}
	p := &Pipe{
		buf:      make([]float32, capacity*format.Channels),
		capacity: capacity,
		format:   format,
	}
	p.notEmpty = sync.NewCond(&p.mu)
	p.notFull = sync.NewCond(&p.mu)
	return p
}

// Format returns the canonical format carried by the pipe.
func (p *Pipe) Format() Format { return p.format }

// Capacity returns the buffer size in frames.
func (p *Pipe) Capacity() int { return p.capacity }

// Available returns the number of frames currently buffered.
func (p *Pipe) Available() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count
}

// Dropped returns how many frames were discarded by Reset calls.
func (p *Pipe) Dropped() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dropped
}

// Free returns the number of frames that can be written without blocking.
func (p *Pipe) Free() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.capacity - p.count
}

// Swap starts a new producer generation: the buffer is dropped and only writes
// carrying the matching token are accepted. token 0 always matches, which keeps
// single-owner uses (tests, tools) simple.
func (p *Pipe) Swap(token uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.token = token
	p.dropped += uint64(p.count)
	p.start, p.count = 0, 0
	p.closed = false
	p.interrupted = false
	p.notFull.Broadcast()
	p.notEmpty.Broadcast()
}

// Token reports the active producer generation.
func (p *Pipe) Token() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.token
}

// Write appends the frames of c under the given generation token.
func (p *Pipe) Write(token uint64, c Chunk) (int, error) {
	return p.write(token, c.Data, c.Frames)
}

// WriteFrames appends frames from an interleaved buffer, blocking on fullness.
func (p *Pipe) WriteFrames(token uint64, data []float32, frames int) (int, error) {
	return p.write(token, data, frames)
}

func (p *Pipe) write(token uint64, data []float32, frames int) (int, error) {
	if frames <= 0 {
		return 0, nil
	}
	ch := p.format.Channels
	if len(data) < frames*ch {
		return 0, errors.New("pcm: short buffer passed to pipe")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if token != 0 && p.token != 0 && token != p.token {
		return 0, ErrPipeStale
	}
	written := 0
	for written < frames {
		if p.closed {
			return written, ErrPipeClosed
		}
		if p.interrupted {
			p.interrupted = false
			return written, ErrPipeInterrupted
		}
		for p.count == p.capacity && !p.closed && !p.interrupted {
			p.notFull.Wait()
		}
		if p.closed {
			return written, ErrPipeClosed
		}
		if p.interrupted {
			p.interrupted = false
			return written, ErrPipeInterrupted
		}
		n := min(p.capacity-p.count, frames-written)
		for i := 0; i < n; i++ {
			idx := (p.start + p.count + i) % p.capacity
			src := (written + i) * ch
			copy(p.buf[idx*ch:(idx+1)*ch], data[src:src+ch])
		}
		p.count += n
		written += n
		p.notEmpty.Signal()
	}
	return written, nil
}

// Read copies buffered frames into dst (interleaved, frames = len(dst)/ch)
// without blocking and returns the number of frames actually read. A short
// read means an underrun: the caller decides whether to pad silence.
func (p *Pipe) Read(dst []float32) int {
	ch := p.format.Channels
	want := len(dst) / ch

	p.mu.Lock()
	defer p.mu.Unlock()
	n := min(want, p.count)
	if n == 0 {
		return 0
	}
	for i := 0; i < n; i++ {
		idx := (p.start + i) % p.capacity
		copy(dst[i*ch:(i+1)*ch], p.buf[idx*ch:(idx+1)*ch])
	}
	p.start = (p.start + n) % p.capacity
	p.count -= n
	if p.count < p.capacity {
		p.notFull.Signal()
	}
	return n
}

// Wait blocks until at least frames frames are buffered, the pipe is closed,
// or a Reset wakes the caller. It returns the available frame count.
func (p *Pipe) Wait(frames int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.count < frames && !p.closed {
		p.notEmpty.Wait()
	}
	return p.count
}

// Interrupt makes the next Write fail fast: the owner bumped a generation and
// the producer must stop feeding audio from the previous one.
func (p *Pipe) Interrupt() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.interrupted = true
	p.notFull.Broadcast()
	p.notEmpty.Broadcast()
}

// Reset drops every buffered frame and wakes blocked producers.
func (p *Pipe) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.interrupted = true
	p.dropped += uint64(p.count)
	p.start = 0
	p.count = 0
	p.notFull.Broadcast()
	p.notEmpty.Broadcast()
}

// Close marks the pipe finished: writers fail fast, readers drain what is left.
func (p *Pipe) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.notFull.Broadcast()
	p.notEmpty.Broadcast()
}

// Wake unblocks producers and waiters so they can re-check their own state
// (used on pause, seek, and shutdown without discarding audio).
func (p *Pipe) Wake() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notFull.Broadcast()
	p.notEmpty.Broadcast()
}

// Reopen clears the closed flag and empties the buffer so the pipe can serve
// the next track or the next output generation.
func (p *Pipe) Reopen() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = false
	p.start = 0
	p.count = 0
	p.notFull.Broadcast()
}
