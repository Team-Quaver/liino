//go:build !windows || (!amd64 && !arm64)

// Package wmf is Windows-only. The stub keeps the name registered so the
// diagnostics UI can explain why the platform decoder is unavailable here.
package wmf

import (
	"context"
	"fmt"
	"runtime"

	"github.com/team-quaver/liino/decode"
)

// Name is the registry key of this backend.
const Name = "wmf"

type backend struct{}

func init() { decode.Register(backend{}) }

func (backend) Name() string  { return Name }
func (backend) Priority() int { return 0 }

func (backend) Probe() (bool, string) {
	return false, fmt.Sprintf("wmf: unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
}

func (backend) Open(context.Context, decode.Source, decode.Options) (decode.Stream, error) {
	return nil, fmt.Errorf("%w: wmf needs windows/amd64 or windows/arm64", decode.ErrUnavailable)
}

func (backend) Codecs() []string { return nil }
