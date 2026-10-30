package dsp

import "math"

// RampGain is a click-free gain stage: every change is ramped over a short
// window instead of being applied instantly, which is what an interactive
// volume slider needs.
type RampGain struct {
	// Gain is the current, already-ramped linear gain.
	Gain float64

	target   float64
	step     float64 // linear gain change per sample
	rate     float64
	rampSecs float64
}

// NewRampGain creates a gain stage starting at unity with a 20 ms default ramp.
func NewRampGain(rate float64) *RampGain {
	if rate <= 0 {
		rate = 48000
	}
	return &RampGain{Gain: 1, target: 1, rate: rate, rampSecs: 0.02}
}

// SetRamp changes how long a gain move takes.
func (g *RampGain) SetRamp(ms float64) {
	if ms < 0 {
		ms = 0
	}
	g.rampSecs = ms / 1000
}

// SetLinear moves the gain towards a linear target (1.0 = unity).
func (g *RampGain) SetLinear(v float64) {
	if v < 0 {
		v = 0
	}
	g.target = v
}

// SetDB moves the gain towards a dB target; -96 dB and below means silence.
func (g *RampGain) SetDB(db float64) {
	if db <= -96 {
		g.SetLinear(0)
		return
	}
	g.SetLinear(math.Pow(10, db/20))
}

// Linear reports the target gain, not the ramping one.
func (g *RampGain) Linear() float64 { return g.target }

// DB reports the target gain in decibels.
func (g *RampGain) DB() float64 {
	if g.target <= 0 {
		return -96
	}
	return 20 * math.Log10(g.target)
}

// Process applies the ramp in place. Gain is shared across channels, so the
// ramp advances once per audio frame rather than once per sample.
func (g *RampGain) Process(data []float32, channels int) {
	if g.rate <= 0 {
		g.rate = 48000
	}
	if channels <= 0 {
		channels = 2
	}
	cur, tgt := g.Gain, g.target
	if cur == tgt {
		if cur == 1 {
			return
		}
		for i := range data {
			data[i] *= float32(cur)
		}
		return
	}
	span := maxF(g.rampSecs*g.rate, 1) // frames the ramp covers
	stepPerFrame := (tgt - cur) / span
	frames := len(data) / channels
	for f := 0; f < frames; f++ {
		cur += stepPerFrame
		if (stepPerFrame > 0 && cur >= tgt) || (stepPerFrame <= 0 && cur <= tgt) {
			cur = tgt
		}
		base := f * channels
		for c := 0; c < channels; c++ {
			if base+c < len(data) {
				data[base+c] *= float32(cur)
			}
		}
		if cur == tgt {
			// Ramp finished: apply the steady gain to whatever is left.
			for i := base + channels; i < len(data); i++ {
				data[i] *= float32(cur)
			}
			break
		}
	}
	g.Gain = cur
}

// Reset snaps the current gain to the target.
func (g *RampGain) Reset() { g.Gain = g.target }

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
