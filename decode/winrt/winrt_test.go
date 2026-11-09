//go:build windows && (amd64 || arm64)

package winrt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/team-quaver/liino/decode"
)

func openFixture(t *testing.T, name string) decode.Stream {
	t.Helper()
	if ok, note := (backend{}).Probe(); !ok {
		t.Skipf("WinRT audio unavailable: %s", note)
	}
	path, err := filepath.Abs(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := (backend{}).Open(ctx, decode.Source{URI: path}, decode.Options{})
	if err != nil {
		t.Fatalf("Open(%s): %v", name, err)
	}
	return st
}

type readResult struct {
	frames int64
	peak   float32
	err    error
}

func readAll(t *testing.T, st decode.Stream, timeout time.Duration) readResult {
	t.Helper()
	done := make(chan readResult, 1)
	go func() {
		var r readResult
		for {
			c, err := st.Read()
			if err != nil {
				r.err = err
				done <- r
				return
			}
			ch := c.Format.Channels
			if ch <= 0 {
				ch = 1
			}
			for _, s := range c.Data[:c.Frames*ch] {
				if s < 0 {
					s = -s
				}
				if s > r.peak {
					r.peak = s
				}
			}
			r.frames += int64(c.Frames)
		}
	}()
	select {
	case r := <-done:
		return r
	case <-time.After(timeout):
		t.Fatalf("read timeout after %v", timeout)
		return readResult{}
	}
}

// TestOpenAutoOgg exercises the real auto path: Ogg is not in Media
// Foundation, and WinRT is the Windows default, so OpenAuto must land here.
func TestOpenAutoOgg(t *testing.T) {
	if ok, note := (backend{}).Probe(); !ok {
		t.Skipf("WinRT audio unavailable: %s", note)
	}
	path, err := filepath.Abs(filepath.Join("..", "..", "testdata", "tone-48000-stereo.ogg"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, b, err := decode.OpenAuto(ctx, decode.Source{URI: path}, decode.Options{})
	if err != nil {
		t.Fatalf("OpenAuto: %v", err)
	}
	defer st.Close()
	t.Logf("selected backend %q", b.Name())
	if b.Name() != Name {
		t.Fatalf("OpenAuto chose %q, want %q", b.Name(), Name)
	}
}

func TestDecodeOgg(t *testing.T) {
	st := openFixture(t, "tone-48000-stereo.ogg")
	defer st.Close()

	if f := st.Format(); f.SampleRate != 48000 || f.Channels != 2 {
		t.Fatalf("format = %+v, want 48000/2", f)
	}
	m := st.Meta()
	t.Logf("meta: codec=%s rate=%d ch=%d dur=%v seekable=%v live=%v",
		m.Codec, m.SampleRate, m.Channels, m.Duration, m.Seekable, m.Live)
	if !m.Seekable {
		t.Fatalf("file source should be seekable: %+v", m)
	}

	r := readAll(t, st, 20*time.Second)
	if !errors.Is(r.err, io.EOF) {
		t.Fatalf("read ended with %v, want EOF", r.err)
	}
	if r.frames <= 0 {
		t.Fatal("no frames decoded")
	}
	if r.peak == 0 {
		t.Fatal("decoded only silence")
	}
	want := int64(m.Duration.Seconds() * 48000)
	t.Logf("decoded %d frames, peak %.3f, duration reports %d frames", r.frames, r.peak, want)
}

// TestPauseResume verifies that stopping the graph when the chunk queue is
// full (the player paused) and restarting it later resumes the source instead
// of replaying it. Duplicated audio would still sum to the reported duration,
// so the samples are compared against an uninterrupted decode.
func TestPauseResume(t *testing.T) {
	const pauseAfter = 24000 // 0.5 s

	baseline := decodeSamples(t, 0, 0)
	paused := decodeSamples(t, pauseAfter, 1500*time.Millisecond)
	if len(baseline) != len(paused) {
		t.Fatalf("paused decode returned %d samples, uninterrupted %d", len(paused), len(baseline))
	}
	for i := range baseline {
		if diff := baseline[i] - paused[i]; diff > 0.001 || diff < -0.001 {
			t.Fatalf("sample %d differs after pause: %f vs %f", i, paused[i], baseline[i])
		}
	}
}

func decodeSamples(t *testing.T, pauseAfterFrames int, pauseFor time.Duration) []float32 {
	t.Helper()
	st := openFixture(t, "tone-48000-stereo.ogg")
	defer st.Close()

	var out []float32
	var frames int64
	deadline := time.Now().Add(20 * time.Second)
	for {
		if pauseAfterFrames > 0 && frames >= int64(pauseAfterFrames) {
			time.Sleep(pauseFor)
			pauseAfterFrames = 0
		}
		c, err := st.Read()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("decode timeout")
		}
		ch := c.Format.Channels
		out = append(out, c.Data[:c.Frames*ch]...)
		frames += int64(c.Frames)
	}
}

// TestDecodeMono keeps the graph at the source's channel count and resamples
// 96 kHz to Liino's canonical 48 kHz.
func TestDecodeMono(t *testing.T) {
	st := openFixture(t, "tone-96000-mono.flac")
	defer st.Close()

	if f := st.Format(); f.SampleRate != 48000 || f.Channels != 1 {
		t.Fatalf("format = %+v, want 48000/1", f)
	}
	if m := st.Meta(); m.SampleRate != 96000 {
		t.Fatalf("meta sample rate = %d, want the source's 96000", m.SampleRate)
	}
	var peak float32
	for i := 0; i < 4; i++ {
		c, err := st.Read()
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if c.Format.Channels != 1 {
			t.Fatalf("chunk channels = %d, want 1", c.Format.Channels)
		}
		for _, s := range c.Data[:c.Frames] {
			if s < 0 {
				s = -s
			}
			if s > peak {
				peak = s
			}
		}
	}
	if peak == 0 {
		t.Fatal("decoded only silence")
	}
}

// TestOpus714Handling covers the multichannel path. Web Media Extensions'
// Opus decoder currently rejects 12-channel (7.1.4) streams with
// MF_E_UNSUPPORTED_FORMAT, in which case the backend must report
// ErrUnsupported so OpenAuto can fall back to ffgo/wasm. If the OS does
// accept the stream, the graph must keep all 12 channels.
func TestOpus714Handling(t *testing.T) {
	if ok, note := (backend{}).Probe(); !ok {
		t.Skipf("WinRT audio unavailable: %s", note)
	}
	path, err := filepath.Abs(filepath.Join("..", "..", "testdata", "tone-48000-71_4.opus"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := (backend{}).Open(ctx, decode.Source{URI: path}, decode.Options{})
	if err != nil {
		if errors.Is(err, decode.ErrUnsupported) {
			t.Skipf("OS Opus decoder cannot open the 12-channel fixture: %v", err)
		}
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	t.Logf("format=%+v meta codec=%s ch=%d dur=%v",
		st.Format(), st.Meta().Codec, st.Meta().Channels, st.Meta().Duration)
	if f := st.Format(); f.SampleRate != 48000 || f.Channels != 12 {
		t.Fatalf("format = %+v, want 48000/12", f)
	}
	var peak float32
	for i := 0; i < 4; i++ {
		c, err := st.Read()
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		for _, s := range c.Data[:c.Frames*c.Format.Channels] {
			if s < 0 {
				s = -s
			}
			if s > peak {
				peak = s
			}
		}
	}
	if peak == 0 {
		t.Fatal("decoded only silence")
	}
}

func TestSeek(t *testing.T) {
	st := openFixture(t, "tone-48000-stereo.ogg")
	defer st.Close()

	if _, err := st.Read(); err != nil {
		t.Fatalf("first Read: %v", err)
	}
	at, err := st.Seek(2 * time.Second)
	if err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if at != 2*time.Second {
		t.Fatalf("Seek reported %v, want 2s", at)
	}
	c, err := st.Read()
	if err != nil {
		t.Fatalf("Read after Seek: %v", err)
	}
	if c.Frames <= 0 {
		t.Fatal("empty chunk after seek")
	}
}

// TestSeekBeforeFirstFrameOverHTTP is the regression for "switch FLAC → Ogg →
// FLAC and the track only plays after dragging the progress bar": Liino opens
// every load with start_ms, so the second FLAC asks for a position change on a
// MediaSource that has not produced audio yet. Windows answers that with
// E_ABORT or blocks for the MediaSource 5 s timeout. warmUp has to run the
// graph until it emits frames before repositioning.
func TestSeekBeforeFirstFrameOverHTTP(t *testing.T) {
	if ok, note := (backend{}).Probe(); !ok {
		t.Skipf("WinRT audio unavailable: %s", note)
	}
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "tone-vbr.flac"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		var start, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return
		}
		if end <= start || end >= len(body) {
			end = len(body) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[start : end+1])
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	url := "http://" + ln.Addr().String() + "/api/stream/flac"

	open := func() decode.Stream {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		st, err := (backend{}).Open(ctx, decode.Source{URI: url, Format: "flac"}, decode.Options{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return st
	}

	// First session primes the MediaSource/HTTP path exactly like the first
	// FLAC of a listening session; close it so the next Open has to build a
	// fresh graph over the same URL.
	first := open()
	if _, err := first.Read(); err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	st := open()
	defer st.Close()
	start := time.Now()
	at, err := st.Seek(800 * time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Seek before first frame: %v", err)
	}
	if at < 700*time.Millisecond || at > 900*time.Millisecond {
		t.Fatalf("Seek reported %v, want ~800ms", at)
	}
	// A cold MediaSource takes its 5 s internal timeout. Keep a generous bound
	// for slow CI machines but far below that.
	if elapsed > 2*time.Second {
		t.Fatalf("Seek before first frame took %v; warm-up is not running", elapsed)
	}
}
