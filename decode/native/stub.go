//go:build android || ios || !(amd64 || arm64)

package native

import (
	"context"
	"fmt"

	"github.com/team-quaver/liino/decode"
)

// ffgo only binds FFmpeg on amd64/arm64 because the purego calling convention
// shims are written for those ABIs. Everywhere else the backend stays
// registered so the UI can still show why it is unavailable.
func (backend) Probe() (bool, string) {
	return false, fmt.Sprintf("ffgo: unsupported platform %s/%s", goOS(), goArch())
}

func (backend) Open(context.Context, decode.Source, decode.Options) (decode.Stream, error) {
	return nil, fmt.Errorf("%w: %s/%s", decode.ErrUnavailable, goOS(), goArch())
}
