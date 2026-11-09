//go:build windows && (amd64 || arm64)

package native

import (
	"net/url"
	"path/filepath"
	"strings"

	"github.com/team-quaver/liino/decode"
)

// PriorityFor moves FFmpeg ahead of the real-time WinRT graph for HTTP Ogg.
//
// WinRT is the correct default when the host has no FFmpeg: Web Media
// Extensions supply Ogg Vorbis/Opus. For Ogg it is also the slow path — an
// initial seek in an Ogg stream needs an index/granule search over HTTP, and
// QQ's "640ogg/320ogg" can be Ogg FLAC, which the Store codec pack does not
// decode. FFmpeg handles both and seeks by byte range. If FFmpeg is not
// installed, Probe fails and OpenAuto walks on to WinRT/WMF exactly as before.
func (backend) PriorityFor(src decode.Source) int {
	if isHTTPOgg(src) {
		return -1
	}

	return 10
}

func isHTTPOgg(src decode.Source) bool {
	if !isHTTPURI(src.URI) {
		return false
	}

	format := strings.ToLower(strings.TrimSpace(src.Format))

	switch {
	case format == "ogg", format == "oga", format == "opus", format == "vorbis":
		return true
	case strings.HasPrefix(format, "audio/ogg"), strings.HasPrefix(format, "application/ogg"):
		return true
	}

	if u, err := url.Parse(src.URI); err == nil {
		switch strings.ToLower(filepath.Ext(u.Path)) {
		case ".ogg", ".oga", ".opus":
			return true
		}
	}

	return false
}

func isHTTPURI(uri string) bool {
	lower := strings.ToLower(uri)

	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}
