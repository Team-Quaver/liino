package dsp

import "math"

// FadeKind selects a fade direction.
type FadeKind int

// Fade directions.
const (
	FadeNone FadeKind = iota
	FadeIn
	FadeOut
)

// Fade ramps the signal to or from silence over a fixed window. It is used for
// cross-track fades and for the fade-out that precedes a stop, so a hard cut
// never reaches the device.
type Fade struct {
	kind     FadeKind
	rate     float64
	seconds  float64
	progress float64 // 0..1
	curve    FadeCurve
}

// FadeCurve shapes the ramp.
type FadeCurve int

// Available curves.
const (
	CurveLinear FadeCurve = iota
	CurveEqualPower
)

// NewFade creates a fade of the given kind and duration.
func NewFade(kind FadeKind, ms, rate float64, curve FadeCurve) *Fade {
	if rate <= 0 {
		rate = 48000
	}
	if ms < 0 {
		ms = 0
	}
	return &Fade{kind: kind, rate: rate, seconds: ms / 1000, curve: curve}
}

// Done reports whether the fade finished.
func (f *Fade) Done() bool {
	if f == nil || f.kind == FadeNone {
		return true
	}
	return f.progress >= 1
}

// Progress reports how far the fade has run (0..1).
func (f *Fade) Progress() float64 { return f.progress }

// Process applies the fade envelope in place.
func (f *Fade) Process(data []float32, channels int) {
	if f == nil || f.kind == FadeNone || f.Done() {
		return
	}
	if channels <= 0 {
		channels = 2
	}
	frames := len(data) / channels
	span := f.seconds * f.rate
	if span <= 0 {
		f.progress = 1
		return
	}
	for i := 0; i < frames; i++ {
		f.progress = math.Min(1, f.progress+1/span)
		g := f.envelope(f.progress)
		base := i * channels
		for c := 0; c < channels && base+c < len(data); c++ {
			data[base+c] *= float32(g)
		}
		if f.progress >= 1 {
			break
		}
	}
}

func (f *Fade) envelope(p float64) float64 {
	switch f.kind {
	case FadeIn:
		if f.curve == CurveEqualPower {
			return math.Sin(p * math.Pi / 2)
		}
		return p
	case FadeOut:
		if f.curve == CurveEqualPower {
			return math.Cos(p * math.Pi / 2)
		}
		return 1 - p
	default:
		return 1
	}
}

// Reset restarts the fade from the beginning.
func (f *Fade) Reset() { f.progress = 0 }

// Skip ends the fade immediately.
func (f *Fade) Skip() { f.progress = 1 }
