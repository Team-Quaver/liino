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
// The decoder preserves the source channel count at 48 kHz.
func drain(t *testing.T, s decode.Stream) (int, float64, float64) {
	t.Helper()
	var frames int
	var sumL, maxAbs float64
	var channels int
	for {
		c, err := s.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if c.Format.SampleRate != pcm.DefaultSampleRate || c.Format.Channels <= 0 {
			t.Fatalf("format = %+v, want 48000 with a channel count", c.Format)
		}
		if channels == 0 {
			channels = c.Format.Channels
		} else if channels != c.Format.Channels {
			t.Fatalf("channel count changed mid-stream: %d -> %d", channels, c.Format.Channels)
		}
		for i := 0; i < c.Frames; i++ {
			for ch := 0; ch < c.Format.Channels; ch++ {
				v := math.Abs(float64(c.Data[i*c.Format.Channels+ch]))
				if v > maxAbs {
					maxAbs = v
				}
			}
			sumL += math.Abs(float64(c.Data[i*c.Format.Channels])) * math.Abs(float64(c.Data[i*c.Format.Channels]))
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

// The decoder keeps mono as one channel; the player duplicates it for the
// endpoint (see pcm.MapChannels). Keeping the source shape here means a mono
// file can still be output as mono on a mono device.
func TestDecodeMonoKeepsLayout(t *testing.T) {
	path := fixture(t, "tone-96000-mono.flac")
	s, err := (backend{}).Open(context.Background(), decode.Source{URI: path}, decode.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Meta().Channels != 1 {
		t.Fatalf("source channels = %d, want 1", s.Meta().Channels)
	}

	frames, rms, peak := drain(t, s)
	if want := 2 * pcm.DefaultSampleRate; absInt(frames-want) > pcm.DefaultSampleRate/10 {
		t.Fatalf("frames = %d, want ~%d", frames, want)
	}
	if rms <= 0 || peak <= 0 {
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

// TestDecode51KeepsLayout covers the wide-layout path: the decoder hands six
// channels to the player, which folds them when the endpoint is stereo.
func TestDecode51KeepsLayout(t *testing.T) {
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
		if c.Format.Channels != 6 {
			t.Fatalf("output channels = %d, want 6 (source layout)", c.Format.Channels)
		}
		// The player's fold must keep both ears alive and distinct.
		stereo := make([]float32, c.Frames*pcm.DefaultChannels)
		pcm.MapChannels(stereo, c.Data[:c.Frames*6], c.Frames, 6, pcm.DefaultChannels)
		for i := 0; i < c.Frames; i++ {
			l, r := math.Abs(float64(stereo[i*2])), math.Abs(float64(stereo[i*2+1]))
			if math.Abs(l-r) > 1e-6 {
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
	if distinct < frames/10 {
		t.Fatalf("downmix produced identical channels (%d/%d frames differ)", distinct, frames)
	}
}

// TestDecode714OpusKeepsLayout is the QQ Music "臻品全景声 7.1" regression:
// 12-channel Opus with mapping family 3 has no channel layout in AVFrame, and
// asking swresample to rematrix it used to fail on the very first frame. The
// decoder must hand all 12 channels through instead.
func TestDecode714OpusKeepsLayout(t *testing.T) {
	path := fixture(t, "tone-48000-71_4.opus")
	s, err := (backend{}).Open(context.Background(), decode.Source{URI: path}, decode.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := s.Meta().Channels; got != 12 {
		t.Fatalf("source channels = %d, want 12", got)
	}
	var frames int
	var peak float32
	var folded bool
	for {
		c, err := s.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if c.Format.Channels != 12 {
			t.Fatalf("output channels = %d, want 12", c.Format.Channels)
		}
		for i := 0; i < c.Frames; i++ {
			// The player's fold must not drop the track either.
			stereo := make([]float32, 2)
			pcm.MapChannels(stereo, c.Data[i*12:i*12+12], 1, 12, 2)
			for _, v := range stereo {
				if v > 1 || v < -1 {
					t.Fatalf("folded sample %v out of range", v)
				}
				if v != 0 {
					folded = true
				}
				if a := float32(math.Abs(float64(v))); a > peak {
					peak = a
				}
			}
		}
		frames += c.Frames
	}
	if frames <= 0 || peak <= 0 || !folded {
		t.Fatalf("no decodable frames (frames=%d peak=%v folded=%v)", frames, peak, folded)
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

// TestCodecsProbe is the Linux capability-detection check: it must report the
// audio decoders the installed FFmpeg was actually built with.
func TestCodecsProbe(t *testing.T) {
	if ok, note := (backend{}).Probe(); !ok {
		t.Skipf("ffgo unavailable: %s", note)
	}
	codecs := (backend{}).Codecs()
	if len(codecs) == 0 {
		t.Fatal("no audio decoders detected")
	}
	t.Logf("detected codecs: %v", codecs)
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
