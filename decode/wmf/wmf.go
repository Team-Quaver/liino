//go:build windows && (amd64 || arm64)

// Package wmf decodes audio through Windows Media Foundation.
//
// The backend uses the Source Reader (IMFSourceReader) with a synchronous
// ReadSample loop. That is deliberate: the Source Reader inserts the OS's own
// demuxer, decoder and audio converter (Media Foundation Transform) chain, so
// MP3/AAC/FLAC/ALAC/WMA and whatever codec pack the machine has installed
// decode in-process with no bundled FFmpeg, no CGO and no helper process.
//
// Asking for float32 PCM at 48 kHz and the source channel count gives Liino the
// same canonical shape as every other backend. A machine without the required
// decoder fails at Open with ErrUnsupported, which is exactly the signal
// decode.OpenAuto uses to fall back to ffgo.
package wmf

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/deploymenttheory/go-bindings-win32/bindings/runtime/win32"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/media/mediafoundation"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/com"

	"github.com/team-quaver/liino/decode"
)

// Name is the registry key of this backend.
const Name = "wmf"

type backend struct{}

func init() { decode.Register(backend{}) }

func (backend) Name() string { return Name }

// Priority is highest: the OS decoder is the zero-dependency path, and it keeps
// DRM-capable/codec-pack content working without shipping FFmpeg.
func (backend) Priority() int { return 0 }

// Codecs lists what the Windows codec pack is expected to provide. Actual
// availability is per machine (N editions need the Media Feature Pack), so
// Probe — not this list — decides whether the backend can run.
func (backend) Codecs() []string {
	return []string{"mp3", "aac", "alac", "flac", "wma", "wav", "ac3", "eac3"}
}

// Supports rejects sources the Source Reader cannot open before we pay for a
// URL fetch. Byte streams are not supported: MFCreateSourceReaderFromURL needs
// a URL (the C ABI already hands Liino URLs, not readers).
func (backend) Supports(src decode.Source) (bool, string) {
	if src.Reader != nil {
		return false, "wmf: byte streams are not supported"
	}
	if src.URI == "" {
		return false, "wmf: empty source"
	}
	if ext := sourceExt(src.URI); ext != "" {
		switch ext {
		// Media Foundation has no demuxer/decoder for these out of the box.
		// Keeping the attempt short lets ffgo (when installed) take over.
		case ".ape", ".wv", ".dsf", ".dff", ".tak", ".tta", ".cue":
			return false, "wmf: unsupported container " + ext
		}
	}
	return true, ""
}

func (backend) Probe() (ok bool, note string) {
	defer func() {
		if r := recover(); r != nil {
			ok, note = false, fmt.Sprintf("media foundation: %v", r)
		}
	}()
	if err := ensureMF(); err != nil {
		return false, "media foundation: " + err.Error()
	}
	return true, "Media Foundation (Source Reader)"
}

func (backend) Open(ctx context.Context, src decode.Source, opts decode.Options) (decode.Stream, error) {
	if src.Reader != nil {
		return nil, fmt.Errorf("%w: wmf needs a URL", decode.ErrUnsupported)
	}
	if err := ensureMF(); err != nil {
		return nil, fmt.Errorf("%w: media foundation: %v", decode.ErrUnavailable, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	release, err := comInit()
	if err != nil {
		return nil, fmt.Errorf("%w: com: %v", decode.ErrUnavailable, err)
	}
	defer release()

	uri, err := sourceURL(src.URI)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", decode.ErrUnsupported, err)
	}

	var reader *mediafoundation.IMFSourceReader
	if err := mediafoundation.MFCreateSourceReaderFromURL(uri, nil, &reader); err != nil {
		return nil, fmt.Errorf("%w: open %q: %v", decode.ErrUnsupported, src.Label(), err)
	}
	keep := false
	defer func() {
		if !keep {
			reader.Release()
		}
	}()

	// Select only the first audio stream. ALL_STREAMS is the -2 sentinel; the
	// uint32 form is written out because a constant conversion of -2 to uint32
	// is a compile-time overflow.
	if err := reader.SetStreamSelection(allStreamsIndex, false); err != nil {
		return nil, fmt.Errorf("%w: stream selection: %v", decode.ErrUnsupported, err)
	}
	if err := reader.SetStreamSelection(audioStreamIndex, true); err != nil {
		return nil, fmt.Errorf("%w: no audio stream: %v", decode.ErrUnsupported, err)
	}

	var native *mediafoundation.IMFMediaType
	if err := reader.GetNativeMediaType(audioStreamIndex, 0, &native); err != nil {
		return nil, fmt.Errorf("%w: no audio stream: %v", decode.ErrUnsupported, err)
	}
	defer native.Release()

	var major win32.GUID
	if err := native.GetMajorType(&major); err != nil {
		return nil, fmt.Errorf("%w: media type: %v", decode.ErrUnsupported, err)
	}
	if major != mediafoundation.MFMediaType_Audio {
		return nil, fmt.Errorf("%w: first stream is not audio", decode.ErrUnsupported)
	}

	var srcRate, srcCh, avgBytes, srcMask uint32
	_ = native.GetUINT32(&mediafoundation.MF_MT_AUDIO_SAMPLES_PER_SECOND, &srcRate)
	_ = native.GetUINT32(&mediafoundation.MF_MT_AUDIO_NUM_CHANNELS, &srcCh)
	_ = native.GetUINT32(&mediafoundation.MF_MT_AUDIO_AVG_BYTES_PER_SECOND, &avgBytes)
	_ = native.GetUINT32(&mediafoundation.MF_MT_AUDIO_CHANNEL_MASK, &srcMask)
	if srcCh == 0 {
		return nil, fmt.Errorf("%w: unknown channel count", decode.ErrUnsupported)
	}
	var sub win32.GUID
	_ = native.GetGUID(&mediafoundation.MF_MT_SUBTYPE, &sub)

	// Ask Media Foundation for canonical float32 PCM. The Source Reader's
	// audio converter MFT resamples; only if it refuses 48 kHz do we decode at
	// the source rate and resample in Go.
	outRate, out := uint32(pcmRate), (*mediafoundation.IMFMediaType)(nil)
	var setErr error
	if out, setErr = outputMediaType(pcmRate, srcCh, srcMask); setErr == nil {
		if err := reader.SetCurrentMediaType(audioStreamIndex, out); err != nil {
			out.Release()
			out, setErr = nil, err
		}
	}
	if out == nil || setErr != nil {
		if srcRate == 0 {
			return nil, fmt.Errorf("%w: no float output type: %v", decode.ErrUnsupported, setErr)
		}
		outRate = srcRate
		var err error
		out, err = outputMediaType(srcRate, srcCh, srcMask)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", decode.ErrUnsupported, err)
		}
		if err := reader.SetCurrentMediaType(audioStreamIndex, out); err != nil {
			out.Release()
			return nil, fmt.Errorf("%w: unsupported channel layout (%d ch): %v", decode.ErrUnsupported, srcCh, err)
		}
	}
	defer out.Release()

	// Read back what was negotiated: some decoders adjust channels.
	var rate, ch uint32
	var current *mediafoundation.IMFMediaType
	if err := reader.GetCurrentMediaType(audioStreamIndex, &current); err == nil && current != nil {
		_ = current.GetUINT32(&mediafoundation.MF_MT_AUDIO_SAMPLES_PER_SECOND, &rate)
		_ = current.GetUINT32(&mediafoundation.MF_MT_AUDIO_NUM_CHANNELS, &ch)
		current.Release()
	}
	if rate == 0 {
		rate = outRate
	}
	if ch == 0 {
		ch = srcCh
	}

	dur := presentationDuration(reader)
	live := src.IsLive()
	meta := decode.Meta{
		Codec:      codecName(sub),
		BitRate:    int64(avgBytes) * 8,
		SampleRate: int(srcRate),
		Channels:   int(srcCh),
		Duration:   dur,
		Seekable:   dur > 0 && !live,
		Live:       live,
	}
	if live {
		meta.Duration = 0
		meta.Seekable = false
	}
	if meta.SampleRate <= 0 {
		meta.SampleRate = int(rate)
	}
	if meta.Channels <= 0 {
		meta.Channels = int(ch)
	}

	keep = true
	return newStream(reader, meta, int(rate), int(ch)), nil
}

const (
	pcmRate          = 48000
	audioStreamIndex = uint32(0xFFFFFFFD) // MF_SOURCE_READER_FIRST_AUDIO_STREAM (-3)
	allStreamsIndex  = uint32(0xFFFFFFFE) // MF_SOURCE_READER_ALL_STREAMS (-2)
	mediaSourceIndex = uint32(0xFFFFFFFF) // MF_SOURCE_READER_MEDIASOURCE (-1)
)

var (
	mfOnce  sync.Once
	mfErr   error
	mtaOnce sync.Once
	mtaKeep = make(chan struct{})
)

// ensureMF starts Media Foundation once per process. MFStartup is reference
// counted; Liino never calls MFShutdown because the host process owns the
// lifetime of every stream.
func ensureMF() error {
	mfOnce.Do(func() {
		ensureMTA()
		mfErr = safeCall(func() error {
			return mediafoundation.MFStartup(mediafoundation.MF_VERSION, mediafoundation.MFSTARTUP_FULL)
		})
	})
	return mfErr
}

// ensureMTA keeps one multithreaded-apartment thread alive for the process.
//
// comInit balances every CoInitializeEx with CoUninitialize, so without a
// permanent reference COM would tear the MTA down between calls while source
// reader objects are still alive. Audio streams outlive any single FFI call,
// hence the keeper goroutine.
func ensureMTA() {
	mtaOnce.Do(func() {
		go func() {
			runtime.LockOSThread()
			_, _ = com.CoInitializeEx(uint32(com.COINIT_MULTITHREADED))
			<-mtaKeep
		}()
	})
}

// comInit enters the multithreaded apartment and pins the goroutine to its OS
// thread for the duration of the call. COM is thread-associated, so every
// source-reader call is wrapped; an MTA object may then be used by any MTA
// thread the Go scheduler picks.
func comInit() (release func(), err error) {
	runtime.LockOSThread()
	hr, err := com.CoInitializeEx(uint32(com.COINIT_MULTITHREADED))
	if err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("CoInitializeEx: %w (hr=0x%08x)", err, uint32(hr))
	}
	return func() {
		com.CoUninitialize()
		runtime.UnlockOSThread()
	}, nil
}

func safeCall(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return fn()
}

// sourceURL turns a local path or file:// URI into the URL form the Source
// Reader expects; http(s) and other schemes pass through untouched.
func sourceURL(uri string) (string, error) {
	if hasScheme(uri) {
		if strings.HasPrefix(strings.ToLower(uri), "file://") {
			path := uri[len("file://"):]
			if len(path) > 2 && path[0] == '/' && path[2] == ':' {
				path = path[1:]
			}
			return fileURL(path), nil
		}
		return uri, nil
	}
	if uri == "" {
		return "", fmt.Errorf("empty source URI")
	}
	abs, err := filepath.Abs(uri)
	if err != nil {
		return "", err
	}
	return fileURL(abs), nil
}

func fileURL(path string) string {
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "file://" + path
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
