//go:build windows && (amd64 || arm64)

package wmf

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/team-quaver/liino/decode"
)

// TestSeekKeepsTheProcessAlive reproduces the Media Foundation seek path.
//
// IMFSourceReader::SetCurrentPosition dereferences its time-format pointer; the
// first implementation passed NULL (meaning "100-ns units" in some MF APIs)
// and every seek - a progress-bar drag, a resume-at-position load - crashed the
// whole process with 0xC0000005. The reader wants a pointer to GUID_NULL.
func TestSeekKeepsTheProcessAlive(t *testing.T) {
	if ok, note := (backend{}).Probe(); !ok {
		t.Skipf("Media Foundation unavailable: %s", note)
	}

	path := filepath.Join(t.TempDir(), "tone.wav")
	writeSilentWAV(t, path, 48000, 2, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := (backend{}).Open(ctx, decode.Source{URI: path}, decode.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if !st.Meta().Seekable {
		t.Fatalf("test WAV should be seekable: %+v", st.Meta())
	}
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
	if _, err := st.Read(); err != nil {
		t.Fatalf("Read after Seek: %v", err)
	}
}

// TestSeekFLACBeforeFirstRead covers the initial seek every resume, quality
// switch and backend switch performs. Media Foundation's FLAC source starts
// its demuxer lazily: SetCurrentPosition before the first ReadSample returns
// success but leaves playback at 0. The clock was then pinned to the requested
// offset, which is exactly "audio restarts from the top while the progress bar
// stays on the breakpoint". seek() must pump the reader once before moving it.
func TestSeekFLACBeforeFirstRead(t *testing.T) {
	if ok, note := (backend{}).Probe(); !ok {
		t.Skipf("Media Foundation unavailable: %s", note)
	}

	path, err := filepath.Abs(filepath.Join("..", "..", "testdata", "tone-96000-mono.flac"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture missing: %v", err)
	}

	fromStart := firstChunk(t, path, 0)
	afterSeek := firstChunk(t, path, 123*time.Millisecond)
	var diff float64
	for i := range fromStart {
		d := math.Abs(float64(afterSeek[i] - fromStart[i]))
		if d > diff {
			diff = d
		}
	}
	t.Logf("sample difference from start after 123ms initial seek: %f", diff)
	if diff < 0.01 {
		t.Fatalf("initial seek did not move the FLAC decoder (max diff %f)", diff)
	}
}

func firstChunk(t *testing.T, path string, at time.Duration) []float32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := (backend{}).Open(ctx, decode.Source{URI: path}, decode.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if at > 0 {
		if _, err := st.Seek(at); err != nil {
			t.Fatalf("Seek(%v): %v", at, err)
		}
	}
	c, err := st.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	out := make([]float32, c.Frames*c.Format.Channels)
	copy(out, c.Data[:len(out)])
	return out
}

// writeSilentWAV writes a 16-bit PCM RIFF/WAVE file, the simplest container
// Media Foundation always demuxes.
func writeSilentWAV(t *testing.T, path string, rate, channels, seconds int) {
	t.Helper()
	frames := rate * seconds
	data := frames * channels * 2
	b := make([]byte, 44+data)
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+data))
	copy(b[8:], "WAVE")
	copy(b[12:], "fmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1) // PCM
	binary.LittleEndian.PutUint16(b[22:], uint16(channels))
	binary.LittleEndian.PutUint32(b[24:], uint32(rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(rate*channels*2))
	binary.LittleEndian.PutUint16(b[32:], uint16(channels*2))
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(data))
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write WAV: %v", err)
	}
}
