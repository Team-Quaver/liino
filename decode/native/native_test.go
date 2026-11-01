package native

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
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

// Network AVIO defaults: a seek is a burst of Range reads, so a 5 s read
// deadline used to turn one slow CDN reply into "Connection timed out" and a
// dead demuxer ("read_timestamp() failed in the middle"). The defaults give
// the read the sidecar's own 15 s upstream budget and let FFmpeg reconnect.
// Caller-supplied avopt values must win.
func TestNetworkAVOptions(t *testing.T) {
	got := networkAVOptions(nil)
	if got["rw_timeout"] != networkReadTimeout {
		t.Fatalf("rw_timeout = %q, want %q", got["rw_timeout"], networkReadTimeout)
	}
	for _, key := range []string{"reconnect", "reconnect_streamed", "reconnect_on_network_error", "reconnect_delay_max", "reconnect_max_retries"} {
		if got[key] == "" {
			t.Fatalf("%s not set in %v", key, got)
		}
	}

	custom := networkAVOptions(map[string]string{"rw_timeout": "1000000", "reconnect": "0"})
	if custom["rw_timeout"] != "1000000" || custom["reconnect"] != "0" {
		t.Fatalf("caller overrides lost: %v", custom)
	}
}

// FLAC fast seeking is only safe when the stream carries a SEEKTABLE: without
// one the demuxer has no index and avformat would fail every seek. The probe
// decides whether fflags=+fastseek is set.
func TestFlacSeekTableDetection(t *testing.T) {
	block := func(typ byte, last bool, n int) []byte {
		h := typ
		if last {
			h |= 0x80
		}
		return append([]byte{h, byte(n >> 16), byte(n >> 8), byte(n)}, make([]byte, n)...)
	}
	write := func(name string, data []byte) string {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	with := []byte("fLaC")
	with = append(with, block(0, false, 34)...) // STREAMINFO
	with = append(with, block(3, false, 18)...) // SEEKTABLE
	with = append(with, block(4, true, 16)...)  // VORBIS_COMMENT (last)
	without := []byte("fLaC")
	without = append(without, block(0, false, 34)...)
	without = append(without, block(4, true, 16)...)

	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"seektable.flac", with, true},
		{"plain.flac", without, false},
		{"notflac.bin", []byte("OggS...."), false},
	}
	for _, tc := range cases {
		path := write(tc.name, tc.data)
		if got := flacHasSeekTable(context.Background(), decode.Source{URI: path}); got != tc.want {
			t.Fatalf("%s: flacHasSeekTable = %v, want %v", tc.name, got, tc.want)
		}
	}
	if flacHasSeekTable(context.Background(), decode.Source{URI: filepath.Join(t.TempDir(), "missing.flac")}) {
		t.Fatal("missing file must not report a seektable")
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// TestDecodeByteSeekOverHTTP covers the network seek fast path: seeking a
// remote source must land on the requested position without FFmpeg's
// timestamp binary search (which is one Range round trip per probe).
func TestDecodeByteSeekOverHTTP(t *testing.T) {
	path := fixture(t, "tone-48000-51.flac")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ServeContent implements Range/206 semantics, matching the host relay.
		http.ServeContent(w, r, "tone.flac", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()

	s, err := (backend{}).Open(context.Background(), decode.Source{URI: srv.URL + "/stream/token"}, decode.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ns, ok := s.(*stream)
	if !ok {
		t.Fatalf("stream type = %T", s)
	}
	if !ns.byteSeek {
		t.Fatal("byte seek path not enabled for an http FLAC source")
	}
	// Seek to the very start first (clicking the first lyric line is a byte
	// offset of 0), then part-way in and verify the decoded tail length.
	if _, err := s.Seek(0); err != nil {
		t.Fatal(err)
	}
	full, _, _ := drain(t, s)
	if want := 2 * pcm.DefaultSampleRate; absInt(full-want) > pcm.DefaultSampleRate/3 {
		t.Fatalf("full frames = %d, want ~%d", full, want)
	}
	if _, err := s.Seek(1500 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	frames, _, _ := drain(t, s)
	// The fixture is 2 s long: seeking to 1.5 s leaves ~0.5 s of audio.
	want := int(0.5 * pcm.DefaultSampleRate)
	if diff := absInt(frames - want); diff > pcm.DefaultSampleRate/3 {
		t.Fatalf("tail frames = %d, want ~%d", frames, want)
	}
}

// TestDecodeByteSeekSkipsMP3 guards the demuxer list: MP3 has no absolute
// timestamps after a byte seek, and falling back to a timestamp seek on that
// demuxer state landed the decoder near EOF instead of the target.
func TestDecodeByteSeekSkipsMP3(t *testing.T) {
	path := fixture(t, "tone-44100-stereo.mp3")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "tone.mp3", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()

	s, err := (backend{}).Open(context.Background(), decode.Source{URI: srv.URL + "/stream/token"}, decode.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if ns, ok := s.(*stream); !ok || ns.byteSeek {
		t.Fatal("byte seek must not be enabled for an MP3 source")
	}
	if _, err := s.Seek(1500 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	frames, _, _ := drain(t, s)
	want := int(1.5 * pcm.DefaultSampleRate)
	if diff := absInt(frames - want); diff > pcm.DefaultSampleRate/3 {
		t.Fatalf("tail frames = %d, want ~%d", frames, want)
	}
}
