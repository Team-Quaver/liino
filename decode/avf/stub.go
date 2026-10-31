//go:build !darwin || (!amd64 && !arm64)

// Package avf is macOS-only. The stub keeps the name registered so the
// diagnostics UI can explain why the platform decoder is unavailable here.
package avf

import (
	"context"
	"fmt"
	"runtime"

	"github.com/team-quaver/liino/decode"
)

// Name is the registry key of this backend.
const Name = "avf"

type backend struct{}

func init() { decode.Register(backend{}) }

func (backend) Name() string  { return Name }
func (backend) Priority() int { return 0 }

func (backend) Probe() (bool, string) {
	return false, fmt.Sprintf("avf: unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
}

func (backend) Open(context.Context, decode.Source, decode.Options) (decode.Stream, error) {
	return nil, fmt.Errorf("%w: avf needs darwin/amd64 or darwin/arm64", decode.ErrUnavailable)
}

func (backend) Codecs() []string { return nil }
