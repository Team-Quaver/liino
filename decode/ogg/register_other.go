//go:build !darwin

// The pure-Go Ogg backend is enabled on macOS only. This stub keeps the name
// registered so diagnostics can explain why it is not used here; other
// platforms already have a platform decoder plus ffgo/libav.js.
package ogg

import (
	"context"
	"fmt"
	"runtime"

	"github.com/team-quaver/liino/decode"
)

func init() { decode.Register(stub{}) }

type stub struct{}

func (stub) Name() string  { return Name }
func (stub) Priority() int { return 5 }
func (stub) Codecs() []string {
	return []string{codecVorbis, codecOpus}
}

func (stub) Probe() (bool, string) {
	return false, fmt.Sprintf("ogg: pure-Go Ogg decoding is enabled on macOS only (this is %s)", runtime.GOOS)
}

func (stub) Supports(decode.Source) (bool, string) { return false, "ogg: unavailable on this platform" }

func (stub) Open(context.Context, decode.Source, decode.Options) (decode.Stream, error) {
	return nil, fmt.Errorf("%w: ogg needs darwin", decode.ErrUnavailable)
}
