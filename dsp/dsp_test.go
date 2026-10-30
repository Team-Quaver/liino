package dsp

import (
	"math"
	"testing"
)

func TestGain(t *testing.T) {
	g := NewGain(-6)
	data := []float32{0.5, -0.5, 1}
	g.Process(data, 2)
	if math.Abs(float64(data[0])-0.251) > 0.001 {
		t.Fatalf("gain = %v", data[0])
	}
}

func TestPeakingUnity(t *testing.T) {
	// 0 dB peaking band must be ~transparent.
	b := NewPeakingEQ(48000, 1000, 1, 0)
	// Alternating input exercises the filter away from DC; with unity gain
	// the output must track the input (after the filter warms up).
	data := []float32{0.25, -0.25, 0.25, -0.25, 0.25, -0.25, 0.25, -0.25, 0.25, -0.25, 0.25, -0.25, 0.25, -0.25, 0.25, -0.25}
	b.Process(data, 2)
	for i, v := range data {
		want := 0.25
		if i%2 == 1 {
			want = -0.25
		}
		if math.Abs(float64(v)-want) > 0.01 {
			t.Fatalf("unity band sample %d = %v, want %v", i, v, want)
		}
	}
}

func TestBiquadStatePerChannel(t *testing.T) {
	b := NewPeakingEQ(48000, 1000, 1, 12)
	left := []float32{0.5, -0.5, 0.5, -0.5, 0.5}
	right := []float32{0.5, -0.5, 0.5, -0.5, 0.5}
	inter := make([]float32, 10)
	for i := range left {
		inter[i*2] = left[i]
		inter[i*2+1] = right[i]
	}
	b.Process(inter, 2)
	for i := range left {
		if inter[i*2] != inter[i*2+1] {
			t.Fatalf("channels diverged at %d", i)
		}
	}
}

func TestLimiterClips(t *testing.T) {
	l := NewLimiter(-1, 1, 50, 48000)
	data := make([]float32, 4800)
	for i := range data {
		data[i] = 1.0 // square wave at unity
	}
	l.Process(data, 2)
	// The envelope follower needs a few ms to catch a unity square wave, so
	// check the settled tail rather than the raw peak.
	tail := data[len(data)-240:]
	max := float32(0)
	for _, v := range tail {
		if v > max {
			max = v
		}
	}
	if max > 0.9 {
		t.Fatalf("limiter did not reduce tail peak: %v", max)
	}
}

func TestChainOrder(t *testing.T) {
	var order []string
	mk := func(name string) Processor {
		return ProcessorFunc(func(data []float32, channels int) {
			order = append(order, name)
		})
	}
	c := Chain{}
	c.Append(mk("a"))
	c.Append(mk("b"))
	c.Process(nil, 2)
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("order = %v", order)
	}
}
