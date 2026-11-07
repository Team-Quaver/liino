//go:build windows && (amd64 || arm64)

// Package winrt decodes audio through the WinRT MediaSource/AudioGraph API.
//
// Windows registers Ogg Vorbis and Opus support through the Store package
// "Web Media Extensions" (Microsoft.WebMediaExtensions). Those components are
// AppX media sources and codec MFTs that live in an app-service hosted
// process. The legacy Media Foundation Source Reader used by package wmf
// cannot activate them from an unpackaged Win32/Electron process - it fails
// with MF_E_UNSUPPORTED_BYTESTREAM_TYPE (0xC00D36C4) even when the extension is
// installed - which is why OGG had no decoder anywhere in the app.
//
// Windows.Media.Core.MediaSource knows how to reach the AppX media source, and
// AudioGraph renders its decoded output. AudioGraph is a real-time API, so this
// backend keeps a bounded chunk queue: the graph runs while the queue has room
// and is stopped when it fills. Read drains the queue, and Seek restarts the
// graph at the new position.
package winrt

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/team-quaver/liino/decode"
)

// Name is the registry key of this backend.
const Name = "winrt"

type backend struct{}

func init() { decode.Register(backend{}) }

func (backend) Name() string { return Name }

// Priority is highest on Windows: WinRT covers the Store codec pack (Ogg/Opus,
// Web Media Extensions) and HTTP FLAC that Media Foundation's Source Reader
// cannot seek. WMF remains the per-source fallback.
func (backend) Priority() int { return 0 }

// Codecs lists what this backend adds over the legacy Source Reader. Actual
// availability depends on the Store codec pack, so Probe and the per-source
// Open result decide.
func (backend) Codecs() []string {
	return []string{"ogg", "vorbis", "opus"}
}

// Supports rejects sources that cannot work before paying for a URI parse and
// a media-source creation.
func (backend) Supports(src decode.Source) (bool, string) {
	if src.Reader != nil {
		return false, "winrt: byte streams are not supported"
	}
	if src.URI == "" {
		return false, "winrt: empty source"
	}
	if ext := sourceExt(src.URI); ext != "" {
		switch ext {
		case ".ape", ".wv", ".dsf", ".dff", ".tak", ".tta", ".cue":
			return false, "winrt: unsupported container " + ext
		}
	}
	return true, ""
}

var probeResult struct {
	once sync.Once
	ok   bool
	note string
}

func (backend) Probe() (ok bool, note string) {
	probeResult.once.Do(func() {
		err := syncMTA(func() error {
			f, err := activationFactory("Windows.Media.Audio.AudioGraph", &iidAudioGraphStatics)
			if err != nil {
				return err
			}
			f.Release()
			return nil
		})
		if err != nil {
			probeResult.ok = false
			probeResult.note = "Windows.Media.Audio: " + err.Error()
			return
		}
		probeResult.ok = true
		probeResult.note = "Windows.Media.Audio (MediaSource/AudioGraph)"
	})
	return probeResult.ok, probeResult.note
}

func (backend) Open(ctx context.Context, src decode.Source, _ decode.Options) (decode.Stream, error) {
	if src.Reader != nil {
		return nil, fmt.Errorf("%w: winrt needs a URI", decode.ErrUnsupported)
	}
	uri, err := mediaURI(src.URI)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", decode.ErrUnsupported, err)
	}
	s := newStream(uri, codecFor(src), src.IsLive())
	go s.openWorker(ctx, 0)
	if err := <-s.ready; err != nil {
		return nil, err
	}
	return s, nil
}

// mediaURI turns a local path or file:// URI into the absolute URI form
// Windows.Foundation.Uri expects. Other schemes pass through untouched.
func mediaURI(uri string) (string, error) {
	if uri == "" {
		return "", fmt.Errorf("empty source URI")
	}
	if hasScheme(uri) {
		if strings.HasPrefix(strings.ToLower(uri), "file://") {
			u, err := url.Parse(uri)
			if err != nil {
				return "", err
			}
			return u.String(), nil
		}
		return uri, nil
	}
	abs, err := filepath.Abs(uri)
	if err != nil {
		return "", err
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String(), nil
}

func codecFor(src decode.Source) string {
	switch sourceExt(src.URI) {
	case ".ogg", ".oga":
		return "ogg"
	case ".opus":
		return "opus"
	case ".webm":
		return "webm"
	case ".mka":
		return "matroska"
	case ".flac":
		return "flac"
	case ".mp3":
		return "mp3"
	case ".m4a", ".aac":
		return "aac"
	}
	if src.Format != "" {
		return strings.ToLower(src.Format)
	}
	return "winrt"
}

func hasScheme(uri string) bool {
	i := strings.Index(uri, "://")
	if i <= 0 {
		return false
	}
	for _, r := range uri[:i] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// sourceExt returns the lower-case extension of a path or URL, if any.
func sourceExt(uri string) string {
	if u, err := url.Parse(uri); err == nil && u.Path != "" {
		return strings.ToLower(filepath.Ext(u.Path))
	}
	if i := strings.IndexAny(uri, "?#"); i >= 0 {
		uri = uri[:i]
	}
	return strings.ToLower(filepath.Ext(uri))
}
