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

func TestMapChannels714ToStereo(t *testing.T) {
	// 7.1.4: L R C LFE Ls Rs Lb Rb Tfl Tfr Tbl Tbr. A front-left-only signal
	// must stay left; a top-left-only signal must also land on the left.
	in := make([]float32, 12)
	in[0] = 0.5
	out := make([]float32, 2)
	MapChannels(out, in, 1, 12, 2)
	if out[0] != 0.5 || out[1] != 0 {
		t.Fatalf("7.1.4 front-left = %v", out)
	}
	clear(in)
	in[8] = 0.4 // top-front-left
	MapChannels(out, in, 1, 12, 2)
	if math.Abs(float64(out[0])-0.28) > 1e-6 || out[1] != 0 {
		t.Fatalf("7.1.4 top-left = %v", out)
	}
	// The fold clamps instead of wrapping when many channels align.
	for i := range in {
		in[i] = 0.9
	}
	MapChannels(out, in, 1, 12, 2)
	for _, v := range out {
		if v > 1 || v < -1 {
			t.Fatalf("folded sample out of range: %v", v)
		}
	}
}

func TestMapChannelsUpmixKeepsFrontPair(t *testing.T) {
	out := make([]float32, 12)
	MapChannels(out, []float32{0.3, -0.3}, 1, 2, 12)
	if out[0] != 0.3 || out[1] != -0.3 {
		t.Fatalf("front pair = %v", out[:2])
	}
	for _, v := range out[2:] {
		if v != 0 {
			t.Fatalf("non-front channel not silent: %v", out[2:])
		}
	}
}

func TestMapChannelsMonoToMultichannel(t *testing.T) {
	out := make([]float32, 6)
	MapChannels(out, []float32{0.4}, 1, 1, 6)
	if out[0] != 0.4 || out[1] != 0.4 {
		t.Fatalf("mono front pair = %v", out[:2])
	}
	for _, v := range out[2:] {
		if v != 0 {
			t.Fatalf("mono tail not silent: %v", out[2:])
		}
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
