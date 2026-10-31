// Package native is Liino's FFmpeg decoder: FFmpeg through ffgo.
//
// ffgo resolves libavformat/libavcodec/libavutil/libswresample at runtime with
// purego, so the whole binary stays CGO_ENABLED=0 while still getting FFmpeg's
// demuxers, decoders and resampler. The cost is that the host must provide the
// FFmpeg shared libraries (system packages on Linux/macOS, DLLs next to the
// executable on Windows).
//
// On Linux this is the primary backend, because there is no single OS media
// decoder to prefer. On Windows and macOS it sits behind the platform backends
// (wmf/avf) by priority and catches the containers those do not ship -
// Ogg/Opus, APE, WavPack, DSD, 7.1.4 Opus, ...
package native

import "github.com/team-quaver/liino/decode"

// Name is the registry key of this backend.
const Name = "ffgo"

type backend struct{}

func init() { decode.Register(backend{}) }

func (backend) Name() string { return Name }

// Priority leaves room for the platform decoder (wmf/avf, priority 0) to be
// preferred on its OS. Linux has no such backend, so ffgo remains the default
// there; Windows and macOS only reach it when the OS codec pack cannot decode
// the source or FFmpeg is unavailable.
func (backend) Priority() int { return 10 }
