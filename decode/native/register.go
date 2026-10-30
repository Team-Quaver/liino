// Package native is the primary Liino decoder: FFmpeg through ffgo.
//
// ffgo resolves libavformat/libavcodec/libavutil/libswresample at runtime with
// purego, so the whole binary stays CGO_ENABLED=0 while still getting FFmpeg's
// demuxers, decoders and resampler. The cost is that the host must provide the
// FFmpeg shared libraries (system packages on Linux/macOS, DLLs next to the
// executable on Windows).
package native

import "github.com/team-quaver/liino/decode"

// Name is the registry key of this backend.
const Name = "ffgo"

type backend struct{}

func init() { decode.Register(backend{}) }

func (backend) Name() string  { return Name }
func (backend) Priority() int { return 0 }
