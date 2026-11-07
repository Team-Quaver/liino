//go:build darwin

package ogg

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/team-quaver/liino/decode"
)

// On macOS AVFoundation rejects Ogg in Supports, so OpenAuto must land on this
// backend before it tries ffgo.
func TestOpenAutoPicksOgg(t *testing.T) {
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
