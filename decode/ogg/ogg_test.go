package ogg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"path/filepath"
	"testing"
	"time"

	gopus "github.com/thesyncim/gopus"
	gogg "github.com/thesyncim/gopus/container/ogg"

	"github.com/team-quaver/liino/decode"
)

func fixture(name string) string {
	p, err := filepath.Abs(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		panic(err)
	}
	return p
}

func open(t *testing.T, src decode.Source) decode.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := (backend{}).Open(ctx, src, decode.Options{})
	if err != nil {
		t.Fatalf("Open(%s): %v", src.Label(), err)
	}
	return st
}

type result struct {
	frames int64
	peak   float32
	err    error
}

func readAll(t *testing.T, st decode.Stream, timeout time.Duration) result {
	t.Helper()
	done := make(chan result, 1)
	go func() {
		var r result
		for {
			c, err := st.Read()
			if err != nil {
				r.err = err
				done <- r
				return
			}
			ch := c.Format.Channels
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
		return result{}
	}
}

func TestVorbisDecode(t *testing.T) {
	st := open(t, decode.Source{URI: fixture("tone-48000-stereo.ogg")})
	defer st.Close()

	if f := st.Format(); f.SampleRate != 48000 || f.Channels != 2 {
		t.Fatalf("format = %+v, want 48000/2", f)
	}
	m := st.Meta()
	t.Logf("meta: codec=%s rate=%d ch=%d dur=%v seekable=%v", m.Codec, m.SampleRate, m.Channels, m.Duration, m.Seekable)
	if m.Codec != codecVorbis {
		t.Fatalf("codec = %q, want %q", m.Codec, codecVorbis)
	}
	if !m.Seekable {
		t.Fatal("local file should be seekable")
	}
	if d := m.Duration; d < 1900*time.Millisecond || d > 2100*time.Millisecond {
		t.Fatalf("duration = %v, want about 2s", d)
	}

	r := readAll(t, st, 20*time.Second)
	if !errors.Is(r.err, io.EOF) {
		t.Fatalf("read ended with %v, want EOF", r.err)
	}
	t.Logf("decoded %d frames, peak %.3f", r.frames, r.peak)
	if r.frames < 95000 || r.frames > 97000 {
		t.Fatalf("decoded %d frames, want about 96000", r.frames)
	}
	if r.peak == 0 {
		t.Fatal("decoded only silence")
	}
}

func TestVorbisSeek(t *testing.T) {
	st := open(t, decode.Source{URI: fixture("tone-48000-stereo.ogg")})
	defer st.Close()

	if _, err := st.Read(); err != nil {
		t.Fatalf("first Read: %v", err)
	}
	at, err := st.Seek(time.Second)
	if err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if at != time.Second {
		t.Fatalf("Seek reported %v, want 1s", at)
	}
	c, err := st.Read()
	if err != nil {
		t.Fatalf("Read after Seek: %v", err)
	}
	if c.Frames == 0 {
		t.Fatal("empty chunk after seek")
	}
}

func TestOpusMultichannel(t *testing.T) {
	st := open(t, decode.Source{URI: fixture("tone-48000-71_4.opus")})
	defer st.Close()

	if f := st.Format(); f.SampleRate != 48000 || f.Channels != 12 {
		t.Fatalf("format = %+v, want 48000/12", f)
	}
	m := st.Meta()
	t.Logf("meta: codec=%s rate=%d ch=%d dur=%v seekable=%v", m.Codec, m.SampleRate, m.Channels, m.Duration, m.Seekable)
	if m.Codec != codecOpus {
		t.Fatalf("codec = %q, want %q", m.Codec, codecOpus)
	}
	if m.Duration < 900*time.Millisecond || m.Duration > 1100*time.Millisecond {
		t.Fatalf("duration = %v, want about 1s", m.Duration)
	}

	r := readAll(t, st, 30*time.Second)
	if !errors.Is(r.err, io.EOF) {
		t.Fatalf("read ended with %v, want EOF", r.err)
	}
	t.Logf("decoded %d frames, peak %.3f", r.frames, r.peak)
	if r.frames < 47000 || r.frames > 49000 {
		t.Fatalf("decoded %d frames, want about 48000", r.frames)
	}
	if r.peak == 0 {
		t.Fatal("decoded only silence")
	}
}

func TestOpusSeek(t *testing.T) {
	st := open(t, decode.Source{URI: fixture("tone-48000-71_4.opus")})
	defer st.Close()

	if _, err := st.Read(); err != nil {
		t.Fatalf("first Read: %v", err)
	}
	at, err := st.Seek(500 * time.Millisecond)
	if err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if at != 500*time.Millisecond {
		t.Fatalf("Seek reported %v, want 500ms", at)
	}
	var peak float32
	for i := 0; i < 4; i++ {
		c, err := st.Read()
		if err != nil {
			t.Fatalf("Read after Seek: %v", err)
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
		t.Fatal("decoded only silence after seek")
	}
}

// TestOpusSeekMatchesDecode checks that restarting the decoder 80 ms early and
// discarding to the exact target reproduces the continuous decode instead of
// clicking or shifting the audio.
func TestOpusSeekMatchesDecode(t *testing.T) {
	const (
		at    = 500 * time.Millisecond
		ch    = 12
		frame = int64(24000) // 500 ms at 48 kHz
		span  = 2400
	)
	baseline := decodeAll(t, fixture("tone-48000-71_4.opus"))
	st := open(t, decode.Source{URI: fixture("tone-48000-71_4.opus")})
	defer st.Close()
	if _, err := st.Seek(at); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	var got []float32
	for len(got)/ch < span {
		c, err := st.Read()
		if err != nil {
			t.Fatalf("Read after Seek: %v", err)
		}
		got = append(got, c.Data[:c.Frames*ch]...)
	}
	var maxDiff float32
	for i := 0; i < span*ch; i++ {
		d := got[i] - baseline[int(frame)*ch+i]
		if d < 0 {
			d = -d
		}
		if d > maxDiff {
			maxDiff = d
		}
	}
	t.Logf("max sample delta after seek: %.5f", maxDiff)
	if maxDiff > 0.02 {
		t.Fatalf("seek diverged from continuous decode: max delta %.5f", maxDiff)
	}
}

// TestOpusSeekStart covers target zero, where the granule coordinate still
// includes the pre-skip and the decoder must not drop a whole packet.
func TestOpusSeekStart(t *testing.T) {
	const ch = 12
	baseline := decodeAll(t, fixture("tone-48000-71_4.opus"))
	st := open(t, decode.Source{URI: fixture("tone-48000-71_4.opus")})
	defer st.Close()
	if _, err := st.Read(); err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if _, err := st.Seek(0); err != nil {
		t.Fatalf("Seek(0): %v", err)
	}
	var got []float32
	for len(got)/ch < 2400 {
		c, err := st.Read()
		if err != nil {
			t.Fatalf("Read after Seek(0): %v", err)
		}
		got = append(got, c.Data[:c.Frames*ch]...)
	}
	var maxDiff float32
	for i := 0; i < 2400*ch; i++ {
		d := got[i] - baseline[i]
		if d < 0 {
			d = -d
		}
		if d > maxDiff {
			maxDiff = d
		}
	}
	t.Logf("max sample delta after Seek(0): %.5f", maxDiff)
	if maxDiff > 0.02 {
		t.Fatalf("Seek(0) diverged from the continuous decode: max delta %.5f", maxDiff)
	}
}

func decodeAll(t *testing.T, path string) []float32 {
	t.Helper()
	st := open(t, decode.Source{URI: path})
	defer st.Close()
	var out []float32
	ch := st.Format().Channels
	for {
		c, err := st.Read()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		out = append(out, c.Data[:c.Frames*ch]...)
	}
}

// TestOpusStream covers mapping family 0 and a non-seekable Source.Reader. The
// fixture set only has a 12-channel file, so the encoder in gopus produces a
// stereo Ogg Opus stream in memory.
func TestOpusStream(t *testing.T) {
	const frames = 50 * 960 // one second
	data := encodeOpus(t, frames)

	st := open(t, decode.Source{Reader: struct{ io.Reader }{bytes.NewReader(data)}})
	defer st.Close()

	if f := st.Format(); f.SampleRate != 48000 || f.Channels != 2 {
		t.Fatalf("format = %+v, want 48000/2", f)
	}
	m := st.Meta()
	if m.Seekable {
		t.Fatal("stream source must not report seekable")
	}
	if m.Duration != 0 {
		t.Fatalf("stream duration = %v, want 0", m.Duration)
	}
	if _, err := st.Seek(time.Second); !errors.Is(err, decode.ErrSeekUnsupported) {
		t.Fatalf("Seek = %v, want ErrSeekUnsupported", err)
	}

	r := readAll(t, st, 20*time.Second)
	if !errors.Is(r.err, io.EOF) {
		t.Fatalf("read ended with %v, want EOF", r.err)
	}
	t.Logf("decoded %d frames, peak %.3f", r.frames, r.peak)
	if r.frames < frames-1000 || r.frames > frames+100 {
		t.Fatalf("decoded %d frames, want about %d", r.frames, frames)
	}
	if r.peak == 0 {
		t.Fatal("decoded only silence")
	}
}

func encodeOpus(t *testing.T, frames int) []byte {
	t.Helper()
	const rate = 48000
	enc, err := gopus.NewEncoder(gopus.EncoderConfig{
		SampleRate:  rate,
		Channels:    2,
		Application: gopus.ApplicationAudio,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := gopus.DefaultDecoderConfig(rate, 2)
	var buf bytes.Buffer
	w, err := gogg.NewWriter(&buf, rate, 2)
	if err != nil {
		t.Fatal(err)
	}
	in := make([]float32, 960*2)
	packet := make([]byte, cfg.MaxPacketBytes)
	phase := 0.0
	for done := 0; done < frames; done += 960 {
		for i := 0; i < 960; i++ {
			v := float32(0.25 * math.Sin(phase))
			in[2*i], in[2*i+1] = v, v
			phase += 2 * math.Pi * 440 / rate
		}
		n, err := enc.Encode(in, packet)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.WritePacket(packet[:n], 960); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestOpenRejectsNonOgg(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := (backend{}).Open(ctx, decode.Source{URI: fixture("tone-44100-stereo.mp3")}, decode.Options{})
	if !errors.Is(err, decode.ErrUnsupported) {
		t.Fatalf("Open(mp3) = %v, want ErrUnsupported", err)
	}
}

func TestSupports(t *testing.T) {
	b := backend{}
	cases := []struct {
		src  decode.Source
		want bool
	}{
		{decode.Source{URI: "/music/a.ogg"}, true},
		{decode.Source{URI: "/music/a.oga"}, true},
		{decode.Source{URI: "/music/a.opus"}, true},
		{decode.Source{URI: "/music/a.OGG"}, true},
		{decode.Source{URI: "/music/a.mp3"}, false},
		{decode.Source{URI: "https://example.com/a.ogg"}, false},
		{decode.Source{Reader: bytes.NewReader(nil)}, true},
		{decode.Source{}, false},
	}
	for _, c := range cases {
		if got, reason := b.Supports(c.src); got != c.want {
			t.Errorf("Supports(%q reader=%v) = %v (%s), want %v", c.src.URI, c.src.Reader != nil, got, reason, c.want)
		}
	}
}
