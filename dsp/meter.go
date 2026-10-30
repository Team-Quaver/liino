package dsp

import "math"

// Level is a read-only snapshot of the signal statistics since the last read.
type Level struct {
	PeakL  float64 `json:"peak_l"`
	PeakR  float64 `json:"peak_r"`
	RMSL   float64 `json:"rms_l"`
	RMSR   float64 `json:"rms_r"`
	Clips  uint64  `json:"clips"`
	Frames uint64  `json:"frames"`
}

// DB returns peak levels in decibels.
func (l Level) DB() (peakL, peakR float64) {
	return linToDB(l.PeakL), linToDB(l.PeakR)
}

func linToDB(v float64) float64 {
	if v <= 0 {
		return -120
	}
	db := 20 * math.Log10(v)
	if db < -120 {
		return -120
	}
	return db
}

// Meter observes the stream without changing it. It is the read-only tap the
// UI uses for level and clipping display; it never blocks the pipeline.
type Meter struct {
	peakL, peakR  float64
	sumL, sumR    float64
	frames, clips uint64
}

// Process samples the buffer and keeps running statistics.
func (m *Meter) Process(data []float32, channels int) {
	if channels <= 0 {
		channels = 2
	}
	frames := len(data) / channels
	for i := 0; i < frames; i++ {
		base := i * channels
		l := math.Abs(float64(data[base]))
		r := l
		if channels > 1 && base+1 < len(data) {
			r = math.Abs(float64(data[base+1]))
		}
		if l > m.peakL {
			m.peakL = l
		}
		if r > m.peakR {
			m.peakR = r
		}
		if l > 1 || r > 1 {
			m.clips++
		}
		m.sumL += l * l
		m.sumR += r * r
		m.frames++
	}
}

// Reset is a no-op: statistics are drained by Read, not by seek.
func (m *Meter) Reset() {}

// Read returns the accumulated window and clears it.
func (m *Meter) Read() Level {
	out := Level{PeakL: m.peakL, PeakR: m.peakR, Clips: m.clips, Frames: m.frames}
	if m.frames > 0 {
		out.RMSL = math.Sqrt(m.sumL / float64(m.frames))
		out.RMSR = math.Sqrt(m.sumR / float64(m.frames))
	}
	m.peakL, m.peakR, m.sumL, m.sumR, m.frames, m.clips = 0, 0, 0, 0, 0, 0
	return out
}

// Peek reports the running window without draining it, so a status poll does
// not starve the caller that actually consumes levels.
func (m *Meter) Peek() Level {
	out := Level{PeakL: m.peakL, PeakR: m.peakR, Clips: m.clips, Frames: m.frames}
	if m.frames > 0 {
		out.RMSL = math.Sqrt(m.sumL / float64(m.frames))
		out.RMSR = math.Sqrt(m.sumR / float64(m.frames))
	}
	return out
}
