package dsp

import (
	"math"
	"sync"
	"testing"
)

// tone builds an interleaved Nyquist-rate square wave, which a high shelf
// attenuates hard — an easy, deterministic way to see whether bands ran at all.
func tone(frames, channels int) []float32 {
	data := make([]float32, frames*channels)
	for frame := 0; frame < frames; frame++ {
		value := float32(0.8)
		if frame%2 == 1 {
			value = -0.8
		}
		for channel := 0; channel < channels; channel++ {
			data[frame*channels+channel] = value
		}
	}
	return data
}

func peak(data []float32) float64 {
	var out float64
	for _, sample := range data {
		out = math.Max(out, math.Abs(float64(sample)))
	}
	return out
}

func TestEQEmptyIsTransparent(t *testing.T) {
	eq := NewEQ(48000)
	data := tone(64, 2)
	want := tone(64, 2)
	eq.Process(data, 2)
	for index := range data {
		if data[index] != want[index] {
			t.Fatalf("empty EQ altered sample %d: %v", index, data[index])
		}
	}
}

func TestEQRetargetIsHeardOnNextProcess(t *testing.T) {
	eq := NewEQ(48000)
	if err := eq.SetBands([]Config{{Freq: 1000, Gain: -24, Shelf: "highshelf"}}); err != nil {
		t.Fatal(err)
	}
	data := tone(240, 2)
	eq.Process(data, 2)
	if got := peak(data[120:]); got > 0.3 {
		t.Fatalf("peak after a -24 dB high shelf = %v, want the band applied", got)
	}

	// Clearing the bands must restore transparency just as fast: this is the
	// "EQ off" toggle.
	if err := eq.SetBands(nil); err != nil {
		t.Fatal(err)
	}
	fresh := tone(64, 2)
	eq.Process(fresh, 2)
	if got := peak(fresh); math.Abs(got-0.8) > 1e-6 {
		t.Fatalf("peak with bands cleared = %v, want 0.8", got)
	}
}

func TestEQBypassKeepsBandsInstalled(t *testing.T) {
	eq := NewEQ(48000)
	if err := eq.SetBands([]Config{{Freq: 1000, Gain: -24, Shelf: "highshelf"}}); err != nil {
		t.Fatal(err)
	}
	eq.SetBypass(true)
	if !eq.Bypass() {
		t.Fatal("bypass not recorded")
	}
	data := tone(240, 2)
	eq.Process(data, 2)
	if got := peak(data); math.Abs(got-0.8) > 1e-6 {
		t.Fatalf("peak while bypassed = %v, want 0.8 (bypass still filtering)", got)
	}
	if len(eq.Bands()) != 1 {
		t.Fatal("bypass dropped the installed bands")
	}

	// Leaving bypass must resume filtering without reinstalling anything.
	eq.SetBypass(false)
	resumed := tone(240, 2)
	eq.Process(resumed, 2)
	if got := peak(resumed[240:]); got > 0.3 {
		t.Fatalf("peak after leaving bypass = %v, want the band applied again", got)
	}
}

func TestEQRejectsInvalidConfigAndKeepsPrevious(t *testing.T) {
	eq := NewEQ(48000)
	good := []Config{{Freq: 1000, Q: 1, Gain: 6}}
	if err := eq.SetBands(good); err != nil {
		t.Fatal(err)
	}
	if err := eq.SetBands([]Config{{Freq: 0, Q: 1}}); err == nil {
		t.Fatal("zero frequency accepted")
	}
	if err := eq.SetBands([]Config{{Freq: 1000, Shelf: "nope"}}); err == nil {
		t.Fatal("unknown shelf accepted")
	}
	if got := eq.Bands(); len(got) != 1 || got[0].Freq != 1000 {
		t.Fatalf("bands after a rejected install = %+v, want the previous set", got)
	}
}

func TestEQDefaultsQAndCopiesConfigs(t *testing.T) {
	eq := NewEQ(48000)
	configs := []Config{{Freq: 500, Gain: 3}}
	if err := eq.SetBands(configs); err != nil {
		t.Fatal(err)
	}
	configs[0].Freq = 9000
	got := eq.Bands()
	if len(got) != 1 || got[0].Freq != 500 || got[0].Q != 1 {
		t.Fatalf("bands = %+v, want freq 500 with Q defaulted to 1", got)
	}
	got[0].Freq = 12000
	if again := eq.Bands(); again[0].Freq != 500 {
		t.Fatalf("Bands handed out its own storage: %+v", again)
	}
}

func TestEQResetDropsFilterMemory(t *testing.T) {
	eq := NewEQ(48000)
	if err := eq.SetBands([]Config{{Freq: 1000, Gain: -24, Shelf: "highshelf"}}); err != nil {
		t.Fatal(err)
	}
	eq.Process(tone(240, 2), 2)
	eq.Reset()
	// After a reset the filter starts from silence again, so the first frame of
	// a fresh buffer must match a brand-new equalizer's first frame.
	eq.Process(tone(240, 2), 2)
	reset := tone(240, 2)
	eq.Reset()
	eq.Process(reset, 2)
	fresh := NewEQ(48000)
	if err := fresh.SetBands([]Config{{Freq: 1000, Gain: -24, Shelf: "highshelf"}}); err != nil {
		t.Fatal(err)
	}
	want := tone(240, 2)
	fresh.Process(want, 2)
	for index := range want {
		if math.Abs(float64(reset[index]-want[index])) > 1e-6 {
			t.Fatalf("reset left filter memory at sample %d: %v vs %v", index, reset[index], want[index])
		}
	}
}

// The audio callback must never wait on a UI-thread call: run the race detector
// over a concurrent retarget while Process is filtering.
func TestEQConcurrentRetarget(t *testing.T) {
	eq := NewEQ(48000)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for step := 0; step < 400; step++ {
			if err := eq.SetBands([]Config{{Freq: 400 + float64(step), Q: 1, Gain: float64(step % 12)}}); err != nil {
				t.Error(err)
				return
			}
			eq.Reset()
		}
	}()
	go func() {
		defer wg.Done()
		data := tone(120, 2)
		for step := 0; step < 400; step++ {
			eq.Process(data, 2)
		}
	}()
	wg.Wait()
}
