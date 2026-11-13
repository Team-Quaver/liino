package dsp

import (
	"errors"
	"sync/atomic"
)

// EQ is the consumer-side equalizer: a band set that can be retargeted from any
// goroutine while Process runs on the audio callback.
//
// It belongs after the jitter ring for the same reason AtomicGain does. The ring
// holds seconds of already-decoded audio, so bands applied before it are not
// heard until that backlog drains — dragging a slider or flipping EQ off looked
// like a multi-second dead zone. Publishing an immutable band set through an
// atomic pointer keeps the audio thread lock-free: it never waits on a mutex the
// host's UI thread holds.
type EQ struct {
	target atomic.Pointer[eqBands]
	// resetReq is bumped by Reset; the audio thread honours it on the next
	// Process. Clearing filter memory directly from a UI call would race with
	// the callback that owns it.
	resetReq atomic.Uint64
	// rate is fixed for the life of the equalizer: it runs on the canonical
	// ring format, which never changes sample rate mid-stream.
	rate float64
	// bypass is the consumer half of Player.SetBypass. The producer chain keeps
	// its own flag; both must agree or a "bypassed" player would still filter.
	bypass atomic.Bool

	// Everything below is audio-thread only.
	filters   []*Biquad
	applied   *eqBands
	resetDone uint64
}

// eqBands is one published, immutable configuration. A new pointer means the
// audio thread must re-tune; the configs slice is never mutated after publishing.
type eqBands struct {
	configs []Config
}

// NewEQ creates an empty (transparent) equalizer.
func NewEQ(rate float64) *EQ {
	if rate <= 0 {
		rate = 48000
	}
	eq := &EQ{rate: rate}
	eq.target.Store(&eqBands{})
	return eq
}

// SetBands publishes a new band list. Safe to call from the UI/FFI thread while
// audio is running: it validates and swaps a pointer, never touching the filters
// the audio callback owns. An invalid band leaves the previous set installed.
func (e *EQ) SetBands(configs []Config) error {
	next := make([]Config, 0, len(configs))
	for _, cfg := range configs {
		if cfg.Freq <= 0 {
			return errors.New("dsp: band frequency must be positive")
		}
		if cfg.Q <= 0 {
			cfg.Q = 1
		}
		// NewBiquad is the authority on which shelf kinds exist; building a
		// throwaway filter here keeps that rule in one place instead of
		// duplicating the switch and letting the two drift.
		if _, err := cfg.NewBiquad(e.rate); err != nil {
			return err
		}
		next = append(next, cfg)
	}
	e.target.Store(&eqBands{configs: next})
	return nil
}

// Bands reports the published configuration.
func (e *EQ) Bands() []Config {
	current := e.target.Load()
	out := make([]Config, 0, len(current.configs))
	return append(out, current.configs...)
}

// Reset asks the audio thread to drop filter memory, e.g. on seek or track
// change. It never blocks and never touches the filters itself.
func (e *EQ) Reset() { e.resetReq.Add(1) }

// SetBypass skips filtering without dropping the installed bands.
func (e *EQ) SetBypass(bypass bool) { e.bypass.Store(bypass) }

// Bypass reports whether filtering is skipped.
func (e *EQ) Bypass() bool { return e.bypass.Load() }

// Process filters the buffer in place. Audio callback only.
func (e *EQ) Process(data []float32, channels int) {
	if e.bypass.Load() {
		return
	}
	if requested := e.resetReq.Load(); requested != e.resetDone {
		e.resetDone = requested
		for _, filter := range e.filters {
			filter.Reset()
		}
	}
	if published := e.target.Load(); published != e.applied {
		e.retune(published)
		e.applied = published
	}
	for _, filter := range e.filters {
		filter.Process(data, channels)
	}
}

// retune rebuilds the filter bank for a published set. Filters keep their
// internal state where the count and kinds match, so dragging a band while
// playing does not click.
func (e *EQ) retune(next *eqBands) {
	previous := e.applied
	filters := make([]*Biquad, 0, len(next.configs))
	for index, cfg := range next.configs {
		if previous != nil && index < len(previous.configs) && index < len(e.filters) &&
			previous.configs[index].Shelf == cfg.Shelf {
			reuse := e.filters[index]
			reuse.Update(cfg.Freq, cfg.Q, cfg.Gain)
			filters = append(filters, reuse)
			continue
		}
		filter, err := cfg.NewBiquad(e.rate)
		if err != nil {
			continue
		}
		filters = append(filters, filter)
	}
	e.filters = filters
}
