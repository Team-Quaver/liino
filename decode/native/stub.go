//go:build android || ios || (!amd64 && !arm64 && !loong64)

package native

import (
	"context"
	"fmt"

	"github.com/team-quaver/liino/decode"
)

// ffgo only binds FFmpeg on the 64-bit ABIs it has been enabled for. Everywhere
// else the backend stays registered so the UI can still show why it is
// unavailable.
func (backend) Probe() (bool, string) {
	return false, fmt.Sprintf("ffgo: unsupported platform %s/%s", goOS(), goArch())
}

func (backend) Open(context.Context, decode.Source, decode.Options) (decode.Stream, error) {
	return nil, fmt.Errorf("%w: %s/%s", decode.ErrUnavailable, goOS(), goArch())
}

// Codecs reports nothing: ffgo cannot load at all on these platforms.
func (backend) Codecs() []string { return nil }
