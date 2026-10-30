package pcm

import (
	"math"
	"testing"
)

func TestRingWriteRead(t *testing.T) {
	r := NewRing(ChunkFrames * 2)
	c := NewChunk(10)
	for i := range c.Data {
		c.Data[i] = float32(i + 1)
	}
	if n := r.Write(c); n != 10 {
		t.Fatalf("write = %d", n)
	}
	got, ok := r.Read(4)
	if !ok || got.Frames != 4 {
		t.Fatalf("read = %d ok=%v", got.Frames, ok)
	}
	for i := range got.Data {
		if got.Data[i] != float32(i+1) {
			t.Fatalf("sample %d = %v", i, got.Data[i])
		}
	}
	if n := r.Write(c); n != 10 {
		t.Fatalf("second write = %d", n)
	}
	got, _ = r.Read(100)
	if got.Frames != 16 {
		t.Fatalf("read max = %d, want 16", got.Frames)
	}
}

func TestRingOverwriteDrops(t *testing.T) {
	r := NewRing(5)
	c := NewChunk(8)
	if n := r.Write(c); n != 5 {
		t.Fatalf("write = %d", n)
	}
	if n := r.Write(c); n != 0 {
		t.Fatalf("full write = %d", n)
	}
}

func TestDownmix(t *testing.T) {
	in := []float32{0.1, 0.2, 0.3, 0.4, 0.5, 0.6}
	out := make([]float32, 4)
	DownmixToStereo(out, in, 2, 3)
	want := []float32{0.1, 0.2, 0.4, 0.5}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("out[%d] = %v", i, out[i])
		}
	}
	mono := make([]float32, 4)
	DownmixToStereo(mono, []float32{0.7, 0.8}, 2, 1)
	if mono[0] != 0.7 || mono[1] != 0.7 || mono[2] != 0.8 || mono[3] != 0.8 {
		t.Fatalf("mono downmix = %v", mono)
	}
}

func TestS16RoundTrip(t *testing.T) {
	src := []float32{0, -1, 0.5, -0.5}
	raw := make([]byte, 8)
	F32ToS16(raw, src, 2, 2)
	back := make([]float32, 4)
	S16ToF32(back, raw, 2, 2)
	if math.Abs(float64(back[2]-0.5)) > 0.001 {
		t.Fatalf("round trip = %v", back)
	}
	if back[1] < -1 || back[1] > -0.99 {
		t.Fatalf("negative clip = %v", back[1])
	}
}

func TestDownmixChannels(t *testing.T) {
	// 5.1 layout is L R C LFE Ls Rs: a left-front-only signal stays on the left.
	out := make([]float32, 2)
	DownmixChannels(out, []float32{0.8, 0, 0, 0, 0, 0}, 1, 6)
	if out[0] != 0.8 || out[1] != 0 {
		t.Fatalf("5.1 left only = %v", out)
	}
	// Centre 0.5 at 0.7 plus LFE 0.6 at 0.5 lands equally on both ears.
	DownmixChannels(out, []float32{0, 0, 0.5, 0.6, 0, 0}, 1, 6)
	want := 0.7*0.5 + 0.5*0.6
	if math.Abs(float64(out[0])-want) > 1e-6 || out[0] != out[1] {
		t.Fatalf("5.1 centre/LFE = %v, want %v", out, want)
	}
	// 5.0: each surround follows its own side.
	DownmixChannels(out, []float32{0, 0, 0, 0.4, 0.2, 0}, 1, 5)
	if out[0] != 0.28 || out[1] != 0.14 {
		t.Fatalf("5.0 downmix = %v", out)
	}
}

func TestStereoCopyIsExact(t *testing.T) {
	in := []float32{0.25, -0.25, 0.5, -0.5}
	out := make([]float32, 4)
	DownmixChannels(out, in, 2, 2)
	for i := range in {
		if in[i] != out[i] {
			t.Fatalf("stereo copy changed sample %d", i)
		}
	}
}
