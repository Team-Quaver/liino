package native

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/team-quaver/liino/decode"
	"github.com/team-quaver/liino/pcm"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	if ok, note := (backend{}).Probe(); !ok {
		t.Skipf("ffgo unavailable: %s", note)
	}
	dir := os.Getenv("LIINO_TESTDATA")
	if dir == "" {
		dir = filepath.Join("..", "..", "testdata")
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture missing (%v); run scripts/fixtures.sh", err)
	}
	return path
}

// drain decodes a whole stream and reports frame count and signal energy.
func drain(t *testing.T, s decode.Stream) (int, float64, float64) {
	t.Helper()
	var frames int
	var sumL, maxAbs float64
	for {
		c, err := s.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if c.Format.Channels != pcm.DefaultChannels || c.Format.SampleRate != pcm.DefaultSampleRate {
			t.Fatalf("format = %+v, want 48000/2", c.Format)
		}
		for i := 0; i < c.Frames; i++ {
			l := math.Abs(float64(c.Data[i*2]))
			r := math.Abs(float64(c.Data[i*2+1]))
			sumL += l * l
			if m := math.Max(l, r); m > maxAbs {
				maxAbs = m
			}
		}
		frames += c.Frames
	}
	rms := 0.0
	if frames > 0 {
		rms = math.Sqrt(sumL / float64(frames))
	}
	return frames, rms, maxAbs
}

func TestDecodeMP3ResamplesToCanonical(t *testing.T) {
	path := fixture(t, "tone-44100-stereo.mp3")
	s, err := (backend{}).Open(context.Background(), decode.Source{URI: path}, decode.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	meta := s.Meta()
	if meta.SampleRate != 44100 {
		t.Fatalf("source rate = %d, want 44100", meta.SampleRate)
	}
	if meta.Channels != 2 {
		t.Fatalf("source channels = %d, want 2", meta.Channels)
	}
	if d := meta.Duration.Round(time.Millisecond); d < 2900*time.Millisecond || d > 3100*time.Millisecond {
		t.Fatalf("duration = %s, want ~3s", d)
	}

	frames, rms, peak := drain(t, s)
	want := 3 * pcm.DefaultSampleRate
	if diff := absInt(frames - want); diff > pcm.DefaultSampleRate/10 {
		t.Fatalf("frames = %d, want ~%d", frames, want)
	}
	if rms < 0.05 || rms > 0.9 {
		t.Fatalf("rms = %v, expected an audible tone", rms)
	}
	if peak <= 0 {
		t.Fatal("decoded silence")
	}
}

func TestDecodeMonoDownmixesToStereo(t *testing.T) {
	path := fixture(t, "tone-96000-mono.flac")
	s, err := (backend{}).Open(context.Background(), decode.Source{URI: path}, decode.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Meta().Channels != 1 {
		t.Fatalf("source channels = %d, want 1", s.Meta().Channels)
	}

	var frames int
	var mismatch int
	var first [2]float32
	for {
		c, err := s.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if frames == 0 {
			first = [2]float32{c.Data[0], c.Data[1]}
		}
		for i := 0; i < c.Frames; i++ {
			// A mono source must land identically on both ears.
			if math.Abs(float64(c.Data[i*2]-c.Data[i*2+1])) > 1e-6 {
				mismatch++
			}
		}
		frames += c.Frames
	}
	if want := 2 * pcm.DefaultSampleRate; absInt(frames-want) > pcm.DefaultSampleRate/10 {
		t.Fatalf("frames = %d, want ~%d", frames, want)
	}
	if mismatch > frames/100 {
		t.Fatalf("mono not duplicated on %d/%d frames", mismatch, frames)
	}
	if first[0] == 0 && first[1] == 0 {
		t.Fatal("silent decode")
	}
}

func TestDecodeSeekReturnsRemainingTail(t *testing.T) {
	path := fixture(t, "tone-44100-stereo.mp3")
	s, err := (backend{}).Open(context.Background(), decode.Source{URI: path}, decode.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Meta().Seekable {
		t.Skip("source not seekable")
	}
	at, err := s.Seek(1500 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if at < 1400*time.Millisecond || at > 1600*time.Millisecond {
		t.Fatalf("seek landed at %s", at)
	}
	frames, _, _ := drain(t, s)
	want := int(1.5 * pcm.DefaultSampleRate)
	if diff := absInt(frames - want); diff > pcm.DefaultSampleRate/3 {
		t.Fatalf("tail frames = %d, want ~%d", frames, want)
	}
}

// TestDecode51DownmixesToStereo covers the wide-layout path: swresample keeps
// six channels and the Go side folds them onto stereo.
func TestDecode51DownmixesToStereo(t *testing.T) {
	path := fixture(t, "tone-48000-51.flac")
	s, err := (backend{}).Open(context.Background(), decode.Source{URI: path}, decode.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	meta := s.Meta()
	if meta.Channels != 6 {
		t.Fatalf("source channels = %d, want 6", meta.Channels)
	}
	if meta.SampleRate != 48000 {
		t.Fatalf("source rate = %d, want 48000", meta.SampleRate)
	}

	var frames, distinct int
	var sumL, sumR float64
	for {
		c, err := s.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if c.Format.Channels != pcm.DefaultChannels {
			t.Fatalf("output channels = %d, want 2", c.Format.Channels)
		}
		for i := 0; i < c.Frames; i++ {
			l, r := math.Abs(float64(c.Data[i*2])), math.Abs(float64(c.Data[i*2+1]))
			if l-r > 1e-6 || r-l > 1e-6 {
				distinct++
			}
			sumL += l
			sumR += r
		}
		frames += c.Frames
	}
	if want := 2 * pcm.DefaultSampleRate; absInt(frames-want) > pcm.DefaultSampleRate/5 {
		t.Fatalf("frames = %d, want ~%d", frames, want)
	}
	if sumL <= 0 || sumR <= 0 {
		t.Fatal("silent downmix")
	}
	// Every source channel holds a different tone, so the folded ears cannot be
	// identical. Equal channels would mean the plane stride was misread.
	if distinct < frames/10 {
		t.Fatalf("downmix produced identical channels (%d/%d frames differ)", distinct, frames)
	}
}

func TestDecodeFromReader(t *testing.T) {
	path := fixture(t, "tone-48000-stereo.ogg")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := (backend{}).Open(context.Background(), decode.Source{Reader: bytes.NewReader(data), Format: "ogg"}, decode.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	frames, rms, _ := drain(t, s)
	if want := 2 * pcm.DefaultSampleRate; absInt(frames-want) > pcm.DefaultSampleRate/5 {
		t.Fatalf("frames = %d, want ~%d", frames, want)
	}
	if rms <= 0 {
		t.Fatal("silent decode from reader")
	}
}

func TestMissingFileFailsFast(t *testing.T) {
	if ok, _ := (backend{}).Probe(); !ok {
		t.Skip("ffgo unavailable")
	}
	_, err := (backend{}).Open(context.Background(), decode.Source{URI: filepath.Join(t.TempDir(), "nope.mp3")}, decode.Options{})
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want a not-exist error", err)
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
