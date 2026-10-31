package decode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/team-quaver/liino/pcm"
)

type fakeBackend struct {
	name     string
	priority int
	probeOK  bool
	supports func(Source) (bool, string)
	openErr  error
	openOK   bool
	codecs   []string
}

func (f fakeBackend) Name() string  { return f.name }
func (f fakeBackend) Priority() int { return f.priority }
func (f fakeBackend) Probe() (bool, string) {
	if f.probeOK {
		return true, "ok"
	}
	return false, "not available"
}
func (f fakeBackend) Supports(src Source) (bool, string) {
	if f.supports == nil {
		return true, ""
	}
	return f.supports(src)
}
func (f fakeBackend) Codecs() []string { return f.codecs }
func (f fakeBackend) Open(context.Context, Source, Options) (Stream, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return fakeStream{name: f.name}, nil
}

type fakeStream struct{ name string }

func (fakeStream) Format() pcm.Format { return pcm.DefaultFormat() }
func (fakeStream) Meta() Meta         { return Meta{} }
func (fakeStream) Read() (pcm.Chunk, error) {
	return pcm.Chunk{}, io.EOF
}
func (fakeStream) Seek(time.Duration) (time.Duration, error) { return 0, ErrSeekUnsupported }
func (fakeStream) Close() error                              { return nil }

func register(t *testing.T, b fakeBackend) {
	t.Helper()
	Register(b)
	t.Cleanup(func() { Unregister(b.name) })
}

// OpenAuto prefers the lower priority number when it can run.
func TestOpenAutoPrefersPriority(t *testing.T) {
	register(t, fakeBackend{name: "test-platform", priority: 0, probeOK: true})
	register(t, fakeBackend{name: "test-fallback", priority: 10, probeOK: true})

	st, b, err := OpenAuto(context.Background(), Source{URI: "/tmp/x.mp3"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "test-platform" {
		t.Fatalf("backend = %s, want test-platform", b.Name())
	}
	if st.(fakeStream).name != "test-platform" {
		t.Fatalf("stream from %s", st.(fakeStream).name)
	}
}

// A platform backend that cannot decode the source must fall through to the
// next usable one instead of failing the track.
func TestOpenAutoFallsBackOnUnsupported(t *testing.T) {
	register(t, fakeBackend{
		name:     "test-unsupported",
		priority: 0,
		probeOK:  true,
		openErr:  fmt.Errorf("%w: ogg", ErrUnsupported),
	})
	register(t, fakeBackend{name: "test-ffmpeg", priority: 10, probeOK: true})

	_, b, err := OpenAuto(context.Background(), Source{URI: "track.ogg"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "test-ffmpeg" {
		t.Fatalf("backend = %s, want test-ffmpeg", b.Name())
	}
}

// Guard.Supports keeps a backend out of the attempt list entirely.
func TestOpenAutoHonoursSupports(t *testing.T) {
	register(t, fakeBackend{
		name:     "test-guard",
		priority: 0,
		probeOK:  true,
		supports: func(Source) (bool, string) { return false, "wrong container" },
	})
	register(t, fakeBackend{name: "test-next", priority: 10, probeOK: true})

	_, b, err := OpenAuto(context.Background(), Source{URI: "track.ogg"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "test-next" {
		t.Fatalf("backend = %s, want test-next", b.Name())
	}
}

// An explicit backend name is a promise: falling back would silently ignore
// the user's choice.
func TestOpenWithExplicitDoesNotFallBack(t *testing.T) {
	register(t, fakeBackend{
		name:     "test-explicit",
		priority: 0,
		probeOK:  true,
		openErr:  fmt.Errorf("%w: nope", ErrUnsupported),
	})
	register(t, fakeBackend{name: "test-should-not-run", priority: 10, probeOK: true})

	_, b, err := OpenWith(context.Background(), "test-explicit", Source{URI: "x"}, Options{})
	if err == nil {
		t.Fatal("expected the explicit backend's error")
	}
	if b == nil || b.Name() != "test-explicit" {
		t.Fatalf("backend = %v, want test-explicit", b)
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

// Diagnostics expose codec detection, including the Linux FFmpeg list.
func TestStatusesReportCodecs(t *testing.T) {
	register(t, fakeBackend{
		name:     "test-codecs",
		priority: 0,
		probeOK:  true,
		codecs:   []string{"mp3", "flac"},
	})
	var found *Status
	for _, st := range Statuses() {
		if st.Name == "test-codecs" {
			s := st
			found = &s
		}
	}
	if found == nil {
		t.Fatal("backend missing from Statuses")
	}
	if len(found.Codecs) != 2 || found.Codecs[0] != "mp3" {
		t.Fatalf("codecs = %v", found.Codecs)
	}
}
