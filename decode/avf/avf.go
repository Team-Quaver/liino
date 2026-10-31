//go:build darwin && (amd64 || arm64)

// Package avf decodes audio through AVFoundation and CoreMedia.
//
// AVURLAsset + AVAssetReader + AVAssetReaderTrackOutput gives Liino the same
// decode path QuickTime and Finder use, backed by the Core Audio codecs macOS
// ships. The output settings ask CoreMedia for interleaved float32 PCM at 48 kHz
// and the source channel count, so the rest of the pipeline sees exactly the
// canonical shape it sees from ffgo or WMF.
//
// The asset reader has no per-stream seek: seeking rebuilds it with a time
// range. That is cheap (it does not re-open the file/codec) and keeps the API
// simple. Containers macOS cannot demux (Ogg/Opus, APE, WavPack, DSD, WMA)
// return ErrUnsupported at Open or are rejected by Supports, which lets
// decode.OpenAuto fall back to ffgo.
package avf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego/objc"

	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/avfaudio"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/avfoundation"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/coreaudiotypes"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/corefoundation"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/coremedia"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/foundation"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/runtime/obj"
	mpurego "github.com/deploymenttheory/go-bindings-macosplatform/bindings/runtime/purego"

	"github.com/team-quaver/liino/decode"
	"github.com/team-quaver/liino/pcm"
)

// Name is the registry key of this backend.
const Name = "avf"

type backend struct{}

func init() { decode.Register(backend{}) }

func (backend) Name() string { return Name }

// Priority is highest on macOS: the OS decoder needs no external runtime.
func (backend) Priority() int { return 0 }

// Codecs lists the Core Audio codecs AVFoundation decodes. The list is what
// macOS ships, not a per-file capability check; Probe decides runnability and
// Open returns ErrUnsupported for a container the system cannot demux.
func (backend) Codecs() []string {
	return []string{"aac", "alac", "mp3", "flac", "pcm", "aiff", "caf", "ac3", "eac3"}
}

// Supports rejects sources that cannot work before AVAssetReader is created.
// The list is deliberately short: a false negative costs a fallback that would
// have worked, so only formats Core Audio definitely lacks are rejected.
func (backend) Supports(src decode.Source) (bool, string) {
	if src.Reader != nil {
		return false, "avf: byte streams are not supported"
	}
	if src.URI == "" {
		return false, "avf: empty source"
	}
	switch sourceExt(src.URI) {
	case ".ogg", ".oga", ".opus", ".webm", ".mka", ".ape", ".wv", ".dsf", ".dff", ".tak", ".tta", ".wma":
		return false, "avf: unsupported container " + sourceExt(src.URI)
	}
	return true, ""
}

func (backend) Probe() (ok bool, note string) {
	if objc.GetClass("AVAssetReader") == 0 {
		return false, "avf: AVFoundation is not available"
	}
	return true, "AVFoundation / CoreMedia"
}

func (backend) Open(ctx context.Context, src decode.Source, opts decode.Options) (decode.Stream, error) {
	if src.Reader != nil {
		return nil, fmt.Errorf("%w: avf needs a URL", decode.ErrUnsupported)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	asset := newAsset(src.URI)
	if asset == nil {
		return nil, fmt.Errorf("%w: cannot open %q", decode.ErrUnsupported, src.Label())
	}
	ok := false
	defer func() {
		if !ok {
			asset.Release()
		}
	}()

	tracks := asset.TracksWithMediaType(avfoundation.AVMediaTypeAudio())
	if len(tracks) == 0 {
		return nil, fmt.Errorf("%w: no audio track", decode.ErrUnsupported)
	}
	track := tracks[0]
	for _, extra := range tracks[1:] {
		extra.Release()
	}

	asbd := trackFormatDescription(track)
	srcRate, srcCh := 0, 0
	codec := ""
	if asbd != nil {
		srcRate = int(math.Round(asbd.MSampleRate))
		srcCh = int(asbd.MChannelsPerFrame)
		codec = codecName(asbd.MFormatID)
	}
	if srcCh <= 0 {
		srcCh = pcm.DefaultChannels
	}
	if srcRate <= 0 {
		srcRate = pcm.DefaultSampleRate
	}

	dur := 0 * time.Second
	if cm := asset.Duration(); cm.Flags&coremedia.KCMTimeFlags_Valid != 0 &&
		cm.Flags&(coremedia.KCMTimeFlags_Indefinite|coremedia.KCMTimeFlags_PositiveInfinity) == 0 {
		if secs := coremedia.CMTimeGetSeconds(cm); !math.IsNaN(secs) && !math.IsInf(secs, 0) && secs > 0 {
			dur = time.Duration(secs * float64(time.Second))
		}
	}
	live := src.IsLive()
	meta := decode.Meta{
		Codec:      codec,
		BitRate:    int64(track.EstimatedDataRate()),
		SampleRate: srcRate,
		Channels:   srcCh,
		Duration:   dur,
		Seekable:   dur > 0 && !live,
		Live:       live,
	}
	if live {
		meta.Duration = 0
		meta.Seekable = false
	}

	s := &stream{
		asset: asset,
		track: track,
		meta:  meta,
		outCh: srcCh,
		chunk: pcm.Chunk{
			Data:   make([]float32, pcm.ChunkFrames*srcCh),
			Format: pcm.Format{SampleRate: pcm.DefaultSampleRate, Channels: srcCh},
		},
	}
	if err := s.openReader(0); err != nil {
		track.Release()
		return nil, err
	}
	ok = true
	return s, nil
}

// sourceExt returns the lower-case extension of a path or URL, if any.
func sourceExt(uri string) string {
	if u, err := url.Parse(uri); err == nil && u.Path != "" && u.Scheme != "" {
		path := u.Path
		if i := strings.LastIndexByte(path, '.'); i >= 0 {
			return strings.ToLower(path[i:])
		}
		return ""
	}
	if i := strings.IndexAny(uri, "?#"); i >= 0 {
		uri = uri[:i]
	}
	if i := strings.LastIndexByte(uri, '.'); i >= 0 {
		ext := strings.ToLower(uri[i:])
		if !strings.ContainsAny(ext, "/\\") {
			return ext
		}
	}
	return ""
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

// newAsset builds AVURLAsset from either a local path or a URL. The generated
// NewURLAssetWithURLOptions always wraps its argument with +fileURLWithPath:,
// which would turn "http://..." into a file URL, so remote URLs are dispatched
// through init:URL:options: directly.
func newAsset(uri string) *avfoundation.URLAsset {
	var nsurl *foundation.URL
	switch {
	case strings.HasPrefix(strings.ToLower(uri), "file://"):
		path := uri[len("file://"):]
		nsurl = foundation.NewURLFileURLWithPath(path)
	case hasScheme(uri):
		nsurl = foundation.NewURLWithString(uri)
	default:
		nsurl = foundation.NewURLFileURLWithPath(uri)
	}
	if nsurl == nil {
		return nil
	}
	defer nsurl.Release()

	cls := objc.GetClass("AVURLAsset")
	if cls == 0 {
		return nil
	}
	alloc := objc.Send[objc.ID](objc.ID(cls), objc.RegisterName("alloc"))
	id := objc.Send[objc.ID](alloc, objc.RegisterName("initWithURL:options:"), obj.ID(nsurl), objc.ID(0))
	if id == 0 {
		return nil
	}
	asset := avfoundation.URLAssetFromID(id)
	// URLAssetFromID retains; drop the +1 from alloc/init.
	objc.Send[objc.ID](id, objc.RegisterName("release"))
	return asset
}

// trackFormatDescription returns the source AudioStreamBasicDescription. It
// describes the stream before decoding, so it is the source of Meta's rate,
// channel count and codec.
func trackFormatDescription(track *avfoundation.AssetTrack) *coreaudiotypes.AudioStreamBasicDescription {
	raw := track.FormatDescriptions()
	if raw == nil {
		return nil
	}
	// FormatDescriptions is documented as an NSArray of CMFormatDescription.
	// Adopting the object directly avoids an isKindOfClass round-trip on the
	// toll-free-bridged CFArray.
	arr := foundation.ArrayFromID(obj.ID(raw))
	raw.Release()
	if arr == nil || arr.Count() == 0 {
		if arr != nil {
			arr.Release()
		}
		return nil
	}
	defer arr.Release()
	desc := arr.ObjectAtIndex(0)
	if desc == nil {
		return nil
	}
	defer desc.Release()
	defer runtime.KeepAlive(desc)
	return coremedia.CMAudioFormatDescriptionGetStreamBasicDescription(objectPointer(desc))
}

// objectPointer exposes a wrapper's Objective-C pointer as the opaque pointer
// the C API takes. obj.ID is the bindings' documented escape hatch; the
// pointer round-trip goes through a variable so go vet's unsafeptr check does
// not mistake it for an arithmetic uintptr conversion.
func objectPointer(o obj.Object) unsafe.Pointer {
	id := obj.ID(o)
	return *(*unsafe.Pointer)(unsafe.Pointer(&id))
}

// FourCC helpers for the Core Audio format IDs.
func fourCC(s string) uint32 {
	if len(s) != 4 {
		return 0
	}
	return uint32(s[0])<<24 | uint32(s[1])<<16 | uint32(s[2])<<8 | uint32(s[3])
}

func codecName(id uint32) string {
	switch id {
	case fourCC("mp3 "):
		return "mp3"
	case fourCC("aac "):
		return "aac"
	case fourCC("alac"):
		return "alac"
	case fourCC("flac"):
		return "flac"
	case fourCC("lpcm"):
		return "pcm"
	case fourCC("ac-3"):
		return "ac3"
	case fourCC("ec-3"):
		return "eac3"
	case fourCC("opus"):
		return "opus"
	case fourCC("vorb"):
		return "vorbis"
	default:
		return fmt.Sprintf("coreaudio:0x%08x", id)
	}
}

// stream is one AVAssetReader session.
type stream struct {
	asset *avfoundation.URLAsset
	track *avfoundation.AssetTrack

	reader *avfoundation.AssetReader
	output *avfoundation.AssetReaderTrackOutput

	meta  decode.Meta
	outCh int

	scratch []float32
	ablBuf  []byte
	chunk   pcm.Chunk

	mu     sync.Mutex
	closed bool
}

func (s *stream) Format() pcm.Format { return s.chunk.Format }
func (s *stream) Meta() decode.Meta  { return s.meta }

// openReader creates a fresh reader. Seeking uses a new reader with a time
// range; the asset and track stay alive.
func (s *stream) openReader(start time.Duration) error {
	reader, err := avfoundation.NewAssetReaderWithAsset(&s.asset.Asset)
	if err != nil {
		return fmt.Errorf("avf: asset reader: %w", err)
	}
	out := avfoundation.NewAssetReaderTrackOutputWithTrackOutputSettings(s.track, outputSettings(s.outCh))
	if out == nil {
		reader.Release()
		return errors.New("avf: cannot create track output")
	}
	ok := false
	defer func() {
		if !ok {
			out.Release()
			reader.Release()
		}
	}()
	if !reader.CanAddOutput(&out.AssetReaderOutput) {
		return errors.New("avf: track output rejected")
	}
	reader.AddOutput(&out.AssetReaderOutput)
	if start > 0 {
		// kCMTimePositiveInfinity's value; constructing it avoids the
		// uintptr-to-unsafe.Pointer conversion of the symbol accessor.
		inf := coremedia.CMTime{
			Value:     1,
			Timescale: 1,
			Flags:     coremedia.KCMTimeFlags_PositiveInfinity | coremedia.KCMTimeFlags_ImpliedValueFlagsMask,
		}
		reader.WithTimeRange(coremedia.CMTimeRange{
			Start:    coremedia.CMTimeMakeWithSeconds(start.Seconds(), 1000),
			Duration: inf,
		})
	}
	if !reader.StartReading() {
		return fmt.Errorf("avf: start reading (%s)", reader.Status())
	}
	s.reader = reader
	s.output = out
	ok = true
	return nil
}

func (s *stream) Read() (pcm.Chunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return pcm.Chunk{}, decode.ErrNotOpen
	}
	for {
		sample := s.output.CopyNextSampleBuffer()
		if sample == nil {
			switch s.reader.Status() {
			case avfoundation.AssetReaderStatusCompleted:
				return pcm.Chunk{}, io.EOF
			case avfoundation.AssetReaderStatusFailed:
				return pcm.Chunk{}, s.readerError()
			case avfoundation.AssetReaderStatusCancelled:
				return pcm.Chunk{}, decode.ErrNotOpen
			default:
				return pcm.Chunk{}, io.EOF
			}
		}
		buf, frames, ch, rate, err := s.decodeSample(sample)
		if err != nil {
			sample.Release()
			return pcm.Chunk{}, err
		}
		if frames <= 0 || ch <= 0 {
			sample.Release()
			continue
		}
		chunk := s.emit(buf, frames, ch, rate)
		sample.Release()
		return chunk, nil
	}
}

// decodeSample copies one CMSampleBuffer into reused interleaved float32
// memory. The sample must stay alive until emit has copied it out.
func (s *stream) decodeSample(sample obj.Object) (buf []float32, frames, ch, rate int, err error) {
	sb := coremedia.CMSampleBufferRef{Object: sample}
	if coremedia.CMSampleBufferDataIsReady(sb) == 0 {
		return nil, 0, 0, 0, nil
	}
	if coremedia.CMSampleBufferGetNumSamples(sb) <= 0 {
		return nil, 0, 0, 0, nil
	}

	asbd := sampleFormat(sb)
	if asbd == nil {
		return nil, 0, 0, 0, errors.New("avf: sample has no audio format description")
	}
	if asbd.MFormatFlags&coreaudiotypes.KAudioFormatFlagIsFloat == 0 || asbd.MBitsPerChannel != 32 {
		return nil, 0, 0, 0, fmt.Errorf("avf: unexpected sample format (flags=0x%x bits=%d)",
			asbd.MFormatFlags, asbd.MBitsPerChannel)
	}
	ch = int(asbd.MChannelsPerFrame)
	rate = int(math.Round(asbd.MSampleRate))
	if ch <= 0 || rate <= 0 {
		return nil, 0, 0, 0, decode.ErrBadFormat
	}

	buf, n, err := s.readBuffers(sb, ch)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	frames = n / ch
	return buf, frames, ch, rate, nil
}

func sampleFormat(sb coremedia.CMSampleBufferRef) *coreaudiotypes.AudioStreamBasicDescription {
	desc := coremedia.CMSampleBufferGetFormatDescription(sb)
	if desc.IsNil() {
		return nil
	}
	// The sample buffer owns the format description, so the ASBD pointer stays
	// valid until the sample is released; drop our extra retain now.
	defer desc.Release()
	return coremedia.CMAudioFormatDescriptionGetStreamBasicDescription(objectPointer(desc.Object))
}

// AudioBufferList layout, as defined by CoreAudioTypes. The list is a count
// followed by that many 16-byte AudioBuffers.
type audioBuffer struct {
	numberChannels uint32
	dataByteSize   uint32
	data           unsafe.Pointer
}

type audioBufferList struct {
	numberBuffers uint32
	buffers       [1]audioBuffer
}

// readBuffers copies the sample's audio buffers into interleaved float32. The
// output settings request interleaved PCM, so the common case is one buffer;
// planar output is interleaved here anyway because a decoder must not assume
// the caller's layout.
func (s *stream) readBuffers(sb coremedia.CMSampleBufferRef, channels int) ([]float32, int, error) {
	if cap(s.ablBuf) < 8+16*8 {
		s.ablBuf = make([]byte, 8+16*8)
	}
	size := len(s.ablBuf)
	var ret, need int
	for {
		ret, need = coremedia.CMSampleBufferGetAudioBufferListWithRetainedBlockBuffer(
			sb, unsafe.Pointer(&s.ablBuf[0]), size,
			corefoundation.CFAllocatorRef{}, corefoundation.CFAllocatorRef{},
			uint32(coremedia.KCMSampleBufferFlag_AudioBufferList_Assure16ByteAlignment), nil)
		if ret == 0 {
			break
		}
		if need <= size {
			return nil, 0, fmt.Errorf("avf: sample buffer list: OSStatus %d", ret)
		}
		size = need
		s.ablBuf = make([]byte, size)
	}

	abl := (*audioBufferList)(unsafe.Pointer(&s.ablBuf[0]))
	nb := int(abl.numberBuffers)
	if nb <= 0 {
		return nil, 0, nil
	}
	buffers := unsafe.Slice(&abl.buffers[0], nb)

	if nb == 1 {
		n := int(buffers[0].dataByteSize) / 4
		if n == 0 || buffers[0].data == nil {
			return nil, 0, nil
		}
		if cap(s.scratch) < n {
			s.scratch = make([]float32, n)
		} else {
			s.scratch = s.scratch[:n]
		}
		copy(s.scratch, unsafe.Slice((*float32)(buffers[0].data), n))
		return s.scratch, n, nil
	}

	// Planar: one buffer per channel.
	if nb != channels {
		return nil, 0, fmt.Errorf("avf: %d planar buffers for %d channels", nb, channels)
	}
	frames := int(buffers[0].dataByteSize) / 4
	if per := int(buffers[0].numberChannels); per > 1 {
		frames /= per
	}
	if frames <= 0 {
		return nil, 0, nil
	}
	n := frames * channels
	if cap(s.scratch) < n {
		s.scratch = make([]float32, n)
	} else {
		s.scratch = s.scratch[:n]
	}
	for ch := 0; ch < channels; ch++ {
		src := unsafe.Slice((*float32)(buffers[ch].data), frames)
		for f := 0; f < frames; f++ {
			s.scratch[f*channels+ch] = src[f]
		}
	}
	return s.scratch, n, nil
}

// emit normalises the decoded rate to the canonical 48 kHz and the source
// channel count.
func (s *stream) emit(buf []float32, frames, ch, rate int) pcm.Chunk {
	if rate == pcm.DefaultSampleRate {
		n := frames * ch
		s.chunk.Data = ensureCap(s.chunk.Data, n)
		copy(s.chunk.Data[:n], buf[:n])
		s.chunk.Frames = frames
		s.chunk.Format = pcm.Format{SampleRate: pcm.DefaultSampleRate, Channels: ch}
		return s.chunk
	}
	want := (frames*pcm.DefaultSampleRate + rate/2) / rate
	s.chunk.Data = ensureCap(s.chunk.Data, want*ch)
	pcm.ResamplePoly(s.chunk.Data[:want*ch], buf[:frames*ch], rate, pcm.DefaultSampleRate, ch, 32)
	s.chunk.Frames = want
	s.chunk.Format = pcm.Format{SampleRate: pcm.DefaultSampleRate, Channels: ch}
	return s.chunk
}

func (s *stream) Seek(t time.Duration) (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, decode.ErrNotOpen
	}
	if !s.meta.Seekable {
		return 0, decode.ErrSeekUnsupported
	}
	if t < 0 {
		t = 0
	}
	if s.meta.Duration > 0 && t >= s.meta.Duration {
		t = s.meta.Duration - time.Millisecond
		if t < 0 {
			t = 0
		}
	}
	s.closeReader()
	if err := s.openReader(t); err != nil {
		return 0, err
	}
	return t, nil
}

func (s *stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.closeReader()
	if s.track != nil {
		s.track.Release()
		s.track = nil
	}
	if s.asset != nil {
		s.asset.Release()
		s.asset = nil
	}
	return nil
}

func (s *stream) closeReader() {
	if s.reader != nil {
		if s.reader.Status() == avfoundation.AssetReaderStatusReading {
			s.reader.CancelReading()
		}
		s.reader.Release()
		s.reader = nil
	}
	if s.output != nil {
		s.output.Release()
		s.output = nil
	}
}

func (s *stream) readerError() error {
	if p := s.reader.Error(); p != nil {
		if e := obj.Wrap(objc.ID(uintptr(p))); e != nil {
			defer e.Release()
			return fmt.Errorf("avf: %s", e.Description())
		}
	}
	return fmt.Errorf("avf: reader status %s", s.reader.Status())
}

// outputSettings asks CoreMedia for interleaved 32-bit float PCM at 48 kHz.
// The dictionary keys are AVFAudio's own constants rather than copied strings,
// so a future rename in the framework cannot silently stop decoding.
func outputSettings(ch int) map[string]obj.Object {
	key := func(o obj.Object) string {
		defer o.Release()
		return mpurego.GoString(obj.ID(o))
	}
	return map[string]obj.Object{
		key(avfaudio.AVFormatIDKey()):               foundation.NewNumberWithUnsignedInt(int(coreaudiotypes.KAudioFormatLinearPCM)),
		key(avfaudio.AVSampleRateKey()):             foundation.NewNumberWithUnsignedInt(pcm.DefaultSampleRate),
		key(avfaudio.AVNumberOfChannelsKey()):       foundation.NewNumberWithUnsignedInt(ch),
		key(avfaudio.AVLinearPCMBitDepthKey()):      foundation.NewNumberWithUnsignedInt(32),
		key(avfaudio.AVLinearPCMIsFloatKey()):       foundation.NewNumberWithBool(true),
		key(avfaudio.AVLinearPCMIsBigEndianKey()):   foundation.NewNumberWithBool(false),
		key(avfaudio.AVLinearPCMIsNonInterleaved()): foundation.NewNumberWithBool(false),
	}
}

func ensureCap(b []float32, n int) []float32 {
	if cap(b) >= n {
		return b[:n]
	}
	return make([]float32, n)
}
