// Package ogg decodes Ogg Vorbis and Ogg Opus entirely in Go.
//
// macOS ships no Ogg demuxer in Core Audio, so AVFoundation rejects these
// files and playback falls through to ffgo or the libav.js helper - neither of
// which works without FFmpeg or Node. This backend closes that gap with two
// pure-Go decoders: jfreymuth/oggvorbis for Vorbis and thesyncim/gopus for Opus
// (including multistream layouts such as 7.1.4). No FFmpeg, no helper process,
// no CGO.
//
// Vorbis is resampled to Liino's canonical 48 kHz; Opus already decodes at
// 48 kHz. The backend is registered on macOS only (see register_darwin.go),
// where it sits between AVFoundation and ffgo. Other platforms keep their
// platform decoder; the stub in register_other.go only reports why the backend
// is unavailable there.
package ogg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/team-quaver/liino/decode"
)

// Name is the registry key of this backend.
const Name = "ogg"

const (
	codecVorbis = "vorbis"
	codecOpus   = "opus"
)

type backend struct{}

func (backend) Name() string { return Name }

// Priority runs after AVFoundation, which cannot demux Ogg, but before ffgo:
// on macOS this decoder needs neither a system FFmpeg nor Node.
func (backend) Priority() int { return 5 }

// Probe is always true: the decoders are compiled in and have no runtime
// dependencies.
func (backend) Probe() (bool, string) {
	return true, "pure Go Ogg Vorbis/Opus"
}

func (backend) Codecs() []string { return []string{codecVorbis, codecOpus} }

// Supports limits the backend to Ogg containers it can demux. The extension is
// only a hint; Open sniffs the first Ogg packet and returns ErrUnsupported for
// other codecs inside an Ogg container (FLAC, Speex, Theora).
func (backend) Supports(src decode.Source) (bool, string) {
	if src.Reader != nil {
		return true, ""
	}
	if src.URI == "" {
		return false, "ogg: empty source"
	}
	if _, ok := localPath(src.URI); !ok {
		return false, "ogg: only local files and byte streams are supported"
	}
	switch ext := sourceExt(src.URI); ext {
	case ".ogg", ".oga", ".opus":
		return true, ""
	default:
		return false, "ogg: unsupported container " + ext
	}
}

func (backend) Open(ctx context.Context, src decode.Source, opts decode.Options) (decode.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, rs, closeFn, err := openSource(src)
	if err != nil {
		return nil, err
	}
	head, codec, serial, err := sniff(r)
	if err != nil {
		_ = closeFn()
		return nil, err
	}
	if rs != nil {
		if _, err := rs.Seek(0, io.SeekStart); err != nil {
			_ = closeFn()
			return nil, fmt.Errorf("ogg: rewind: %w", err)
		}
	} else {
		// The sniff consumed the start of a one-shot stream; put it back.
		r = io.MultiReader(bytes.NewReader(head), r)
	}
	if opts.Log != nil {
		opts.Log(fmt.Sprintf("ogg: %s stream from %s", codec, src.Label()))
	}

	var st decode.Stream
	switch codec {
	case codecVorbis:
		st, err = newVorbis(r, rs != nil, closeFn)
	case codecOpus:
		st, err = newOpus(r, rs, serial, closeFn)
	default:
		err = fmt.Errorf("%w: unknown Ogg codec", decode.ErrUnsupported)
	}
	if err != nil {
		_ = closeFn()
		return nil, err
	}
	return st, nil
}

// openSource returns a reader, its seeker when available, and a cleanup
// function. Local files are opened directly; Source.Reader is used as-is.
func openSource(src decode.Source) (io.Reader, io.ReadSeeker, func() error, error) {
	if src.Reader != nil {
		rs, _ := src.Reader.(io.ReadSeeker)
		return src.Reader, rs, func() error { return nil }, nil
	}
	path, ok := localPath(src.URI)
	if !ok {
		return nil, nil, nil, fmt.Errorf("%w: ogg needs a local file", decode.ErrUnsupported)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, err
	}
	return f, f, f.Close, nil
}

// localPath resolves a plain path or file:// URL and rejects anything else.
// Remote sources have to arrive as a Source.Reader; http(s) decoding is left to
// ffgo, which knows how to follow the range requests seeking needs.
func localPath(uri string) (string, bool) {
	if strings.HasPrefix(strings.ToLower(uri), "file:") {
		u, err := url.Parse(uri)
		if err != nil || u.Path == "" {
			return "", false
		}
		p := u.Path
		// file:///C:/music/track.ogg parses with a leading slash.
		if len(p) >= 3 && p[0] == '/' && p[2] == ':' &&
			((p[1] >= 'A' && p[1] <= 'Z') || (p[1] >= 'a' && p[1] <= 'z')) {
			p = p[1:]
		}
		return p, true
	}
	// A Windows drive path must not be mistaken for a URL scheme.
	if len(uri) >= 2 && uri[1] == ':' &&
		((uri[0] >= 'A' && uri[0] <= 'Z') || (uri[0] >= 'a' && uri[0] <= 'z')) {
		return uri, true
	}
	if u, err := url.Parse(uri); err == nil && u.Scheme != "" {
		return "", false
	}
	return uri, true
}

// sourceExt returns the lower-case extension of a path or URL, including the
// dot, or "" when there is none.
func sourceExt(uri string) string {
	p := uri
	if u, err := url.Parse(uri); err == nil && u.Scheme != "" {
		p = u.Path
	}
	i := strings.LastIndexByte(p, '.')
	if i < 0 || strings.ContainsAny(p[i:], `/\`) {
		return ""
	}
	return strings.ToLower(p[i:])
}

// sniff reads just enough of the first Ogg page to identify the codec and
// returns the consumed prefix so a non-seekable source can be replayed. The
// serial number comes from the same page and lets the Opus path find the last
// granule position.
func sniff(r io.Reader) (head []byte, codec string, serial uint32, err error) {
	read := func(n int) ([]byte, error) {
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, fmt.Errorf("%w: truncated Ogg header", decode.ErrUnsupported)
			}
			return nil, err
		}
		head = append(head, b...)
		return b, nil
	}

	hdr, err := read(27)
	if err != nil {
		return nil, "", 0, err
	}
	if string(hdr[:4]) != "OggS" {
		return nil, "", 0, fmt.Errorf("%w: not an Ogg stream", decode.ErrUnsupported)
	}
	if hdr[4] != 0 {
		return nil, "", 0, fmt.Errorf("%w: Ogg version %d", decode.ErrUnsupported, hdr[4])
	}
	serial = uint32(hdr[14]) | uint32(hdr[15])<<8 | uint32(hdr[16])<<16 | uint32(hdr[17])<<24

	seg, err := read(int(hdr[26]))
	if err != nil {
		return nil, "", 0, err
	}
	plen := 0
	for _, v := range seg {
		plen += int(v)
		if v < 255 {
			break
		}
	}
	if plen < 7 {
		return nil, "", 0, fmt.Errorf("%w: empty Ogg identification header", decode.ErrUnsupported)
	}
	need := plen
	if need > 8 {
		need = 8
	}
	pkt, err := read(need)
	if err != nil {
		return nil, "", 0, err
	}

	switch {
	case len(pkt) >= 8 && string(pkt[:8]) == "OpusHead":
		return head, codecOpus, serial, nil
	case len(pkt) >= 7 && pkt[0] == 1 && string(pkt[1:7]) == "vorbis":
		return head, codecVorbis, serial, nil
	default:
		return nil, "", 0, fmt.Errorf("%w: Ogg codec %q", decode.ErrUnsupported, printable(pkt))
	}
}

// printable names the identification packet for the error message without
// dumping arbitrary bytes into the UI.
func printable(b []byte) string {
	n := len(b)
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			n = i
			break
		}
	}
	if n == 0 {
		return fmt.Sprintf("% x", b)
	}
	return string(b[:n])
}

// ensureCap resizes b to n floats, reallocating only when it has to grow.
func ensureCap(b []float32, n int) []float32 {
	if cap(b) >= n {
		return b[:n]
	}
	return make([]float32, n)
}
