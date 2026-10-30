// Package dsp implements the Liino post-decode effects chain. All processors
// operate in place on interleaved float32 PCM.
package dsp

import (
	"fmt"
	"math"
)

// Processor is a single in-place effects stage.
type Processor interface {
	Process(data []float32, channels int)
	Reset()
}

// Chain applies a list of processors in order.
type Chain struct {
	Processors []Processor
}

func (c *Chain) Process(data []float32, channels int) {
	for _, p := range c.Processors {
		if p != nil {
			p.Process(data, channels)
		}
	}
}

func (c *Chain) Reset() {
	for _, p := range c.Processors {
		if p != nil {
			p.Reset()
		}
	}
}

// Append adds a processor to the chain.
func (c *Chain) Append(p Processor) { c.Processors = append(c.Processors, p) }

// Gain scales samples by a linear gain factor.
type Gain struct {
	Gain float64 // linear, 1.0 = unity
}

func NewGain(db float64) *Gain {
	return &Gain{Gain: math.Pow(10, db/20)}
}

func (g *Gain) Process(data []float32, _ int) {
	if g.Gain == 1 {
		return
	}
	for i := range data {
		data[i] *= float32(g.Gain)
	}
}

func (g *Gain) Reset() {}

// Biquad is a direct-form-1 second-order IIR filter (RBJ cookbook). It powers
// the EQ bands; each instance is one peaking band.
type Biquad struct {
	kind               string
	rate               float64
	freq               float64
	q                  float64
	gainDB             float64
	b0, b1, b2, a1, a2 float64
	state              []bqState
}

type bqState struct {
	x1, x2, y1, y2 float64
}

// NewPeakingEQ creates an RBJ peaking filter.
func NewPeakingEQ(rate, freq, q, gainDB float64) *Biquad {
	b := &Biquad{kind: "peaking", rate: rate, freq: freq, q: q, gainDB: gainDB}
	b.update()
	return b
}

// NewLowShelf and NewHighShelf create RBJ shelving filters.
func NewLowShelf(rate, freq, gainDB float64) *Biquad {
	return newShelf(rate, freq, gainDB, "lowshelf")
}
func NewHighShelf(rate, freq, gainDB float64) *Biquad {
	return newShelf(rate, freq, gainDB, "highshelf")
}

func newShelf(rate, freq, gainDB float64, kind string) *Biquad {
	b := &Biquad{kind: kind, rate: rate, freq: freq, gainDB: gainDB, q: 0.707}
	b.update()
	return b
}

func (b *Biquad) update() {
	rate := b.rate
	if rate <= 0 {
		rate = 48000
	}
	freq := b.freq
	if freq <= 0 {
		freq = 1000
	}
	omega := 2 * math.Pi * freq / rate
	alpha := math.Sin(omega) / (2 * b.q)
	A := math.Pow(10, b.gainDB/40)
	cosw := math.Cos(omega)
	switch b.kind {
	case "lowshelf":
		sqrtA := 2 * math.Sqrt(A) * alpha
		b.b0 = A * ((A + 1) - (A-1)*cosw + sqrtA)
		b.b1 = 2 * A * ((A - 1) - (A+1)*cosw)
		b.b2 = A * ((A + 1) - (A-1)*cosw - sqrtA)
		a0 := (A + 1) + (A-1)*cosw + sqrtA
		a1 := -2 * ((A - 1) + (A+1)*cosw)
		a2 := (A + 1) + (A-1)*cosw - sqrtA
		b.normalize(a0, a1, a2)
	case "highshelf":
		sqrtA := 2 * math.Sqrt(A) * alpha
		b.b0 = A * ((A + 1) + (A-1)*cosw + sqrtA)
		b.b1 = -2 * A * ((A - 1) + (A+1)*cosw)
		b.b2 = A * ((A + 1) + (A-1)*cosw - sqrtA)
		a0 := (A + 1) - (A-1)*cosw + sqrtA
		a1 := 2 * ((A - 1) - (A+1)*cosw)
		a2 := (A + 1) - (A-1)*cosw - sqrtA
		b.normalize(a0, a1, a2)
	default: // peaking
		b.b0 = 1 + alpha*A
		b.b1 = -2 * cosw
		b.b2 = 1 - alpha*A
		a0 := 1 + alpha/A
		a1 := -2 * cosw
		a2 := 1 - alpha/A
		b.normalize(a0, a1, a2)
	}
}

// Update recomputes the coefficients in place and keeps the filter state, so
// dragging an EQ band does not restart the response or click.
func (b *Biquad) Update(freq, q, gainDB float64) {
	if freq > 0 {
		b.freq = freq
	}
	if q > 0 {
		b.q = q
	}
	b.gainDB = gainDB
	b.update()
}

// Params reports the filter configuration.
func (b *Biquad) Params() (freq, q, gainDB float64, kind string) {
	return b.freq, b.q, b.gainDB, b.kind
}

// SetRate rebuilds the coefficients for a new sample rate.
func (b *Biquad) SetRate(rate float64) {
	if rate <= 0 {
		return
	}
	b.rate = rate
	b.update()
}

func (b *Biquad) normalize(a0, a1, a2 float64) {
	b.b0 /= a0
	b.b1 /= a0
	b.b2 /= a0
	b.a1 = a1 / a0
	b.a2 = a2 / a0
}

// Process applies the filter per-channel (state is channel-interleaved).
func (b *Biquad) Process(data []float32, channels int) {
	if channels <= 0 {
		channels = 2
	}
	ch := channels
	if len(b.state) < ch {
		b.state = make([]bqState, ch)
	}
	state := b.state
	for i := 0; i < len(data); i += ch {
		for c := 0; c < ch; c++ {
			if i+c >= len(data) {
				break
			}
			x := float64(data[i+c])
			s := &state[c]
			y := b.b0*x + b.b1*s.x1 + b.b2*s.x2 - b.a1*s.y1 - b.a2*s.y2
			s.x2, s.x1 = s.x1, x
			s.y2, s.y1 = s.y1, y
			data[i+c] = float32(y)
		}
	}
}

func (b *Biquad) Reset() { clear(b.state) }

// Config describes an EQ band.
type Config struct {
	Freq  float64
	Q     float64
	Gain  float64 // dB
	Shelf string  // "", "lowshelf", "highshelf"
}

// NewBiquad builds a processor from Config.
func (c Config) NewBiquad(rate float64) (*Biquad, error) {
	switch c.Shelf {
	case "", "peaking":
		return NewPeakingEQ(rate, c.Freq, c.Q, c.Gain), nil
	case "lowshelf":
		return NewLowShelf(rate, c.Freq, c.Gain), nil
	case "highshelf":
		return NewHighShelf(rate, c.Freq, c.Gain), nil
	default:
		return nil, fmt.Errorf("dsp: unknown shelf type %q", c.Shelf)
	}
}

// Limiter is a simple lookahead-free soft clipper with attack/release.
type Limiter struct {
	Threshold float64
	Attack    float64
	Release   float64
	rate      float64
	env       float64
}

func NewLimiter(thresholdDB, attackMs, releaseMs, rate float64) *Limiter {
	return &Limiter{
		Threshold: math.Pow(10, thresholdDB/20),
		Attack:    attackMs / 1000,
		Release:   releaseMs / 1000,
		rate:      rate,
	}
}

// Process applies a simple envelope follower and soft clip above threshold.
func (l *Limiter) Process(data []float32, channels int) {
	if channels <= 0 {
		channels = 2
	}
	if l.rate <= 0 {
		l.rate = 48000
	}
	thr := l.Threshold
	if thr <= 0 {
		return
	}
	coefA := 1 - math.Exp(-1/(l.rate*math.Max(l.Attack, 0.000001)))
	coefR := 1 - math.Exp(-1/(l.rate*math.Max(l.Release, 0.000001)))
	env := l.env
	// Process per-sample over the interleaved buffer (channels share env).
	for i := 0; i < len(data); i++ {
		abs := math.Abs(float64(data[i]))
		env += (abs - env) * coefA
		if env > abs {
			env += (abs - env) * coefR
		}
		if env > thr {
			over := (env - thr) / (1 - thr)
			scale := 1 / (1 + over)
			data[i] *= float32(scale)
		}
	}
	l.env = env
}

func (l *Limiter) Reset() { l.env = 0 }
