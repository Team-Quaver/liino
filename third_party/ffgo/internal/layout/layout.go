//go:build !ios && !android && (amd64 || arm64)

// Package layout resolves the in-memory offsets of FFmpeg's public structs.
//
// purego cannot ask a C compiler for offsetof(), so every field access has to
// know where the field lives. Those offsets move whenever FFmpeg inserts or
// removes a field, which it does in every major release. Everything here was
// measured with offsetof() against the matching FFmpeg release:
//
//	avutil 57 = FFmpeg 5.1, 58 = 6.x, 59 = 7.x, 60 = 8.x
//
// Two access patterns are used on purpose:
//
//   - exact tables for the releases above;
//   - buffer geometry (data pointers, linesize, nb_samples, sample format) as a
//     cross-check and as a fallback for releases newer than the last measured
//     table. The leading AVFrame fields have not moved between 5.1 and 8.x, so
//     geometry keeps working even when a table is stale.
package layout

import (
	"strconv"
	"sync"
	"unsafe"

	"github.com/bnema/purego"

	"github.com/team-quaver/ffgo/internal/bindings"
)

// Absent marks a field that does not exist in this FFmpeg version.
const Absent = -1

// Frame holds AVFrame field offsets.
type Frame struct {
	Data         int // uint8_t *data[AV_NUM_DATA_POINTERS]
	Linesize     int // int linesize[AV_NUM_DATA_POINTERS]
	ExtendedData int // uint8_t **extended_data
	Width        int
	Height       int
	NbSamples    int
	Format       int // enum AVSampleFormat for audio frames
	PTS          int
	SampleRate   int
	ChLayout     int // AVChannelLayout: .order at +0, .nb_channels at +4
	Channels     int // deprecated int channels (removed in FFmpeg 6)
	KeyFrame     int // deprecated int key_frame (removed in FFmpeg 8)
	Flags        int // int flags, AV_FRAME_FLAG_*
}

// Par holds AVCodecParameters field offsets.
type Par struct {
	CodecType   int
	CodecID     int
	CodecTag    int
	Extradata   int
	ExtradataSz int
	Format      int
	BitRate     int
	Width       int
	Height      int
	SampleRate  int
	ChLayout    int
	Channels    int // deprecated int channels (removed in FFmpeg 7)
	FrameSize   int
	BlockAlign  int
}

// Ctx holds AVFormatContext field offsets. FFmpeg 7 moved the chapter fields to
// the front of the struct and shifted duration, bit_rate and metadata along.
type Ctx struct {
	IFormat    int
	OFormat    int
	PB         int
	NbStreams  int
	Streams    int
	StartTime  int
	Duration   int
	BitRate    int
	NbChapters int
	Chapters   int
	NbPrograms int
	Programs   int
	Metadata   int
	ProbeScore int
}

// Stream holds AVStream field offsets. FFmpeg 6 reordered the struct: up to 5.1
// the public fields started at index/id and codecpar sat at the very end.
type Stream struct {
	Index        int
	ID           int
	CodecPar     int
	TimeBase     int
	Duration     int
	NbFrames     int
	Metadata     int
	AvgFrameRate int
}

// Table is the measured layout for one FFmpeg release.
type Table struct {
	Label  string
	Exact  bool // the offsets were measured for the running library
	Frame  Frame
	Par    Par
	Ctx    Ctx
	Stream Stream
}

// Offsets shared by every supported release, verified with offsetof() on
// 5.1.8, 6.1.3, 7.1.1 and 8.1.3. Only fields beyond these differ.
const (
	frameData         = 0  // uint8_t *data[8]
	frameLinesize     = 64 // int linesize[8]
	frameExtendedData = 96 // uint8_t **extended_data
	frameWidth        = 104
	frameHeight       = 108
	frameNbSamples    = 112
	frameFormat       = 116
	framePTS          = 136

	parCodecType   = 0
	parCodecID     = 4
	parCodecTag    = 8
	parExtradata   = 16
	parExtradataSz = 24

	ctxIFormat   = 8
	ctxOFormat   = 16
	ctxPB        = 32
	ctxNbStreams = 44
	ctxStreams   = 48

	channelLayoutNbChannels = 4 // AVChannelLayout.nb_channels, right after .order
	avFrameFlagKey          = 2 // 1 << 1

	maxPlanes   = 8 // AV_NUM_DATA_POINTERS
	maxChannels = 64
)

// frameTable builds the AVFrame layout for one release.
func frameTable(sampleRate, chLayout, channels, keyFrame, flags int) Frame {
	return Frame{
		Data: frameData, Linesize: frameLinesize, ExtendedData: frameExtendedData,
		Width: frameWidth, Height: frameHeight, NbSamples: frameNbSamples,
		Format: frameFormat, PTS: framePTS,
		SampleRate: sampleRate, ChLayout: chLayout, Channels: channels,
		KeyFrame: keyFrame, Flags: flags,
	}
}

// parTable builds the AVCodecParameters layout for one release.
func parTable(format, bitRate, width, sampleRate, chLayout, channels, frameSize int) Par {
	return Par{
		CodecType: parCodecType, CodecID: parCodecID, CodecTag: parCodecTag,
		Extradata: parExtradata, ExtradataSz: parExtradataSz,
		Format: format, BitRate: bitRate, Width: width, Height: width + 4,
		SampleRate: sampleRate, ChLayout: chLayout, Channels: channels,
		FrameSize: frameSize, BlockAlign: frameSize - 4,
	}
}

// ctxTable builds the AVFormatContext layout for one release.
func ctxTable(startTime, duration, bitRate, nbChapters, chapters, nbPrograms, programs, metadata, probeScore int) Ctx {
	return Ctx{
		IFormat: ctxIFormat, OFormat: ctxOFormat, PB: ctxPB,
		NbStreams: ctxNbStreams, Streams: ctxStreams,
		StartTime: startTime, Duration: duration, BitRate: bitRate,
		NbChapters: nbChapters, Chapters: chapters,
		NbPrograms: nbPrograms, Programs: programs,
		Metadata: metadata, ProbeScore: probeScore,
	}
}

// streamTable builds the AVStream layout for one release.
func streamTable(index, id, codecPar, timeBase, duration, nbFrames, metadata, avgFrameRate int) Stream {
	return Stream{
		Index: index, ID: id, CodecPar: codecPar, TimeBase: timeBase,
		Duration: duration, NbFrames: nbFrames, Metadata: metadata,
		AvgFrameRate: avgFrameRate,
	}
}

var tables = map[int]Table{
	57: { // FFmpeg 5.1
		Label:  "FFmpeg 5.1 (libavutil 57)",
		Frame:  frameTable(208, 448, 380, 120, 316),
		Par:    parTable(28, 32, 56, 116, 144, 112, 124),
		Ctx:    ctxTable(64, 72, 80, 164, 168, 132, 136, 176, 300),
		Stream: streamTable(0, 4, 208, 16, 32, 40, 64, 72),
	},
	58: { // FFmpeg 6.0 / 6.1
		Label:  "FFmpeg 6.x (libavutil 58)",
		Frame:  frameTable(208, 448, Absent, 120, 316),
		Par:    parTable(28, 32, 56, 116, 144, 112, 124),
		Ctx:    ctxTable(64, 72, 80, 164, 168, 132, 136, 176, 300),
		Stream: streamTable(8, 12, 16, 32, 48, 56, 80, 88),
	},
	59: { // FFmpeg 7.x
		Label:  "FFmpeg 7.x (libavutil 59)",
		Frame:  frameTable(192, 408, Absent, 120, 292),
		Par:    parTable(44, 48, 72, 152, 128, Absent, 160),
		Ctx:    ctxTable(96, 104, 112, 72, 80, 164, 168, 192, 324),
		Stream: streamTable(8, 12, 16, 32, 48, 56, 80, 88),
	},
	60: { // FFmpeg 8.x
		Label:  "FFmpeg 8.x (libavutil 60)",
		Frame:  frameTable(180, 384, Absent, Absent, 276),
		Par:    parTable(44, 48, 72, 152, 128, Absent, 160),
		Ctx:    ctxTable(96, 104, 112, 72, 80, 164, 168, 192, 324),
		Stream: streamTable(8, 12, 16, 32, 48, 56, 80, 88),
	},
}

// CtxField names an AVFormatContext field.
type CtxField int

const (
	CtxPB CtxField = iota
	CtxNbStreams
	CtxStreams
	CtxDuration
	CtxBitRate
	CtxStartTime
	CtxMetadata
	CtxNbChapters
	CtxChapters
	CtxNbPrograms
	CtxPrograms
	CtxProbeScore
	CtxIFormat
	CtxOFormat
)

// CtxOffset returns the offset of an AVFormatContext field, or Absent.
func CtxOffset(f CtxField) int {
	t := Resolve()
	if t == nil {
		return Absent
	}
	switch f {
	case CtxPB:
		return t.Ctx.PB
	case CtxNbStreams:
		return t.Ctx.NbStreams
	case CtxStreams:
		return t.Ctx.Streams
	case CtxDuration:
		return t.Ctx.Duration
	case CtxBitRate:
		return t.Ctx.BitRate
	case CtxStartTime:
		return t.Ctx.StartTime
	case CtxMetadata:
		return t.Ctx.Metadata
	case CtxNbChapters:
		return t.Ctx.NbChapters
	case CtxChapters:
		return t.Ctx.Chapters
	case CtxNbPrograms:
		return t.Ctx.NbPrograms
	case CtxPrograms:
		return t.Ctx.Programs
	case CtxProbeScore:
		return t.Ctx.ProbeScore
	case CtxIFormat:
		return t.Ctx.IFormat
	case CtxOFormat:
		return t.Ctx.OFormat
	}
	return Absent
}

// StreamField names an AVStream field.
type StreamField int

const (
	StreamIndex StreamField = iota
	StreamID
	StreamCodecPar
	StreamTimeBase
	StreamDuration
	StreamNbFrames
	StreamMetadata
	StreamAvgFrameRate
)

// StreamOffset returns the offset of an AVStream field, or Absent.
func StreamOffset(f StreamField) int {
	t := Resolve()
	if t == nil {
		return Absent
	}
	switch f {
	case StreamIndex:
		return t.Stream.Index
	case StreamID:
		return t.Stream.ID
	case StreamCodecPar:
		return t.Stream.CodecPar
	case StreamTimeBase:
		return t.Stream.TimeBase
	case StreamDuration:
		return t.Stream.Duration
	case StreamNbFrames:
		return t.Stream.NbFrames
	case StreamMetadata:
		return t.Stream.Metadata
	case StreamAvgFrameRate:
		return t.Stream.AvgFrameRate
	}
	return Absent
}

// ParField names an AVCodecParameters field.
type ParField int

const (
	ParFFormat ParField = iota
	ParFBitRate
	ParFWidth
	ParFHeight
	ParFExtradata
	ParFExtradataSize
	ParFSampleRate
	ParFFrameSize
)

// ParOffset returns the offset of an AVCodecParameters field, or Absent.
func ParOffset(f ParField) int {
	t := Resolve()
	if t == nil {
		return Absent
	}
	switch f {
	case ParFFormat:
		return t.Par.Format
	case ParFBitRate:
		return t.Par.BitRate
	case ParFWidth:
		return t.Par.Width
	case ParFHeight:
		return t.Par.Height
	case ParFExtradata:
		return t.Par.Extradata
	case ParFExtradataSize:
		return t.Par.ExtradataSz
	case ParFSampleRate:
		return t.Par.SampleRate
	case ParFFrameSize:
		return t.Par.FrameSize
	}
	return Absent
}

var (
	mu       sync.RWMutex
	resolved *Table
)

// Resolve returns the layout for the loaded FFmpeg, falling back to the newest
// measured table when the running release is unknown.
func Resolve() *Table {
	mu.RLock()
	t := resolved
	mu.RUnlock()
	if t != nil {
		return t
	}

	mu.Lock()
	defer mu.Unlock()
	if resolved != nil {
		return resolved
	}
	major := int(bindings.AVUtilVersion() >> 16)
	if tab, ok := tables[major]; ok {
		tab.Exact = true
		resolved = &tab
		return resolved
	}
	// Unknown release: use the newest table we measured. Readers validate what
	// they read and channel counts can still be derived from buffer geometry,
	// so a stale table degrades instead of breaking.
	fallback, ok := tables[60]
	if !ok {
		return nil
	}
	fallback.Exact = false
	fallback.Label = "FFmpeg (libavutil " + strconv.Itoa(major) + ", unmeasured)"
	resolved = &fallback
	return resolved
}

// FrameChannels returns the channel count of a decoded audio frame. Plane
// geometry wins over the layout field because it describes the buffers that are
// about to be read; the layout field still covers wide channel counts.
func FrameChannels(f unsafe.Pointer) int32 {
	if f == nil {
		return 0
	}
	geom := geometryChannels(f)
	tab := tableChannels(f)
	if tab > maxPlanes && geom >= maxPlanes {
		return tab
	}
	if geom > 0 {
		return geom
	}
	return tab
}

// FrameSampleRate returns AVFrame.sample_rate, or 0 when it cannot be trusted.
// A frame without a rate is legal; the caller falls back to the stream.
func FrameSampleRate(f unsafe.Pointer) int32 {
	t := Resolve()
	if t == nil || f == nil {
		return 0
	}
	r := readI32(f, t.Frame.SampleRate)
	if !plausibleRate(r) {
		return 0
	}
	return r
}

// FrameNbSamples returns AVFrame.nb_samples.
func FrameNbSamples(f unsafe.Pointer) int32 {
	if f == nil {
		return 0
	}
	return readI32(f, frameNbSamples)
}

// FrameFormat returns AVFrame.format: the sample format for audio frames.
func FrameFormat(f unsafe.Pointer) int32 {
	if f == nil {
		return Absent
	}
	return readI32(f, frameFormat)
}

// FramePTS returns AVFrame.pts.
func FramePTS(f unsafe.Pointer) int64 {
	if f == nil {
		return NoPTS
	}
	return readI64(f, framePTS)
}

// NoPTS is AV_NOPTS_VALUE.
const NoPTS = int64(^uint64(0)>>63) * -1

// FrameIsKey reports AV_FRAME_FLAG_KEY, falling back to the legacy key_frame.
func FrameIsKey(f unsafe.Pointer) bool {
	t := Resolve()
	if t == nil || f == nil {
		return false
	}
	if t.Frame.KeyFrame != Absent {
		return readI32(f, t.Frame.KeyFrame) != 0
	}
	if t.Frame.Flags != Absent {
		return readI32(f, t.Frame.Flags)&avFrameFlagKey != 0
	}
	return false
}

// SetFrameSampleRate writes AVFrame.sample_rate. It is a no-op when the offset
// for the running library is unknown.
func SetFrameSampleRate(f unsafe.Pointer, rate int32) {
	t := Resolve()
	if t == nil || f == nil {
		return
	}
	writeI32(f, t.Frame.SampleRate, rate)
}

// SetFrameChannels sets AVFrame's channel count. FFmpeg 5.1+ keeps it inside
// AVChannelLayout, which also carries the layout mask, so the library helper
// fills the struct in instead of poking a single int.
func SetFrameChannels(f unsafe.Pointer, channels int32) {
	t := Resolve()
	if t == nil || f == nil || channels <= 0 {
		return
	}
	if t.Frame.ChLayout != Absent {
		bindLib()
		if avChannelLayoutDefault != nil {
			avChannelLayoutDefault(uintptr(f)+uintptr(t.Frame.ChLayout), channels)
			return
		}
		writeI32(f, t.Frame.ChLayout+channelLayoutNbChannels, channels)
		return
	}
	if t.Frame.Channels != Absent {
		writeI32(f, t.Frame.Channels, channels)
	}
}

// ParSampleRate returns AVCodecParameters.sample_rate, or 0 if implausible.
func ParSampleRate(par unsafe.Pointer) int32 {
	t := Resolve()
	if t == nil || par == nil {
		return 0
	}
	r := readI32(par, t.Par.SampleRate)
	if !plausibleRate(r) {
		return 0
	}
	return r
}

// ParChannels returns the channel count of an AVCodecParameters, or 0.
func ParChannels(par unsafe.Pointer) int32 {
	t := Resolve()
	if t == nil || par == nil {
		return 0
	}
	if t.Par.ChLayout != Absent {
		if n := readI32(par, t.Par.ChLayout+channelLayoutNbChannels); n > 0 && n <= maxChannels {
			return n
		}
	}
	if t.Par.Channels != Absent {
		if n := readI32(par, t.Par.Channels); n > 0 && n <= maxChannels {
			return n
		}
	}
	return 0
}

// ParSampleFormat returns AVCodecParameters.format (sample format for audio).
func ParSampleFormat(par unsafe.Pointer) int32 {
	t := Resolve()
	if t == nil || par == nil {
		return Absent
	}
	return readI32(par, t.Par.Format)
}

// ParFrameSize returns AVCodecParameters.frame_size: the decoder's hint for
// samples per channel per frame, or 0.
func ParFrameSize(par unsafe.Pointer) int32 {
	t := Resolve()
	if t == nil || par == nil {
		return 0
	}
	if n := readI32(par, t.Par.FrameSize); n > 0 {
		return n
	}
	return 0
}

func tableChannels(f unsafe.Pointer) int32 {
	t := Resolve()
	if t == nil {
		return 0
	}
	if t.Frame.ChLayout != Absent {
		if n := readI32(f, t.Frame.ChLayout+channelLayoutNbChannels); n > 0 && n <= maxChannels {
			return n
		}
	}
	if t.Frame.Channels != Absent {
		if n := readI32(f, t.Frame.Channels); n > 0 && n <= maxChannels {
			return n
		}
	}
	return 0
}

// geometryChannels counts populated sample planes for planar formats and
// divides the stride for packed ones. Unknown input yields 0.
func geometryChannels(f unsafe.Pointer) int32 {
	format := readI32(f, frameFormat)
	if format < 0 {
		return 0
	}
	planes := planeCount(f)
	if isPlanar(format) {
		return planes
	}
	nb := readI32(f, frameNbSamples)
	if nb <= 0 || planes == 0 {
		return 0
	}
	bps := bytesPerSample(format)
	if bps <= 0 {
		return 0
	}
	stride := readI32(f, frameLinesize)
	if stride <= 0 || stride%(nb*bps) != 0 {
		return 0 // aligned padding: geometry cannot decide
	}
	ch := stride / (nb * bps)
	if ch <= 0 || ch > maxChannels {
		return 0
	}
	return int32(ch)
}

func planeCount(f unsafe.Pointer) int32 {
	data := (*[maxPlanes]unsafe.Pointer)(unsafe.Add(f, frameData))
	if ext := readPtr(f, frameExtendedData); ext != nil && uintptr(ext) != uintptr(f)+frameData {
		// More than AV_NUM_DATA_POINTERS planes: extended_data owns the array.
		data = (*[maxPlanes]unsafe.Pointer)(ext)
	}
	var n int32
	for i := 0; i < maxPlanes; i++ {
		if data[i] == nil {
			break
		}
		n++
	}
	return n
}

func plausibleRate(r int32) bool { return r >= 300 && r <= 1000000 }

func readI32(p unsafe.Pointer, off int) int32 {
	if p == nil || off < 0 {
		return 0
	}
	return *(*int32)(unsafe.Add(p, off))
}

func readI64(p unsafe.Pointer, off int) int64 {
	if p == nil || off < 0 {
		return 0
	}
	return *(*int64)(unsafe.Add(p, off))
}

func readPtr(p unsafe.Pointer, off int) unsafe.Pointer {
	if p == nil || off < 0 {
		return nil
	}
	return *(*unsafe.Pointer)(unsafe.Add(p, off))
}

func writeI32(p unsafe.Pointer, off int, v int32) {
	if p == nil || off < 0 {
		return
	}
	*(*int32)(unsafe.Add(p, off)) = v
}

// Sample format helpers come from libavutil so that planar detection does not
// depend on the numeric enum, which grows over release lines.
var (
	bindOnce               sync.Once
	avGetBytesPerSample    func(format int32) int32
	avGetSampleFmtName     func(format int32) unsafe.Pointer
	avChannelLayoutDefault func(chLayout uintptr, nbChannels int32)
)

func bindLib() {
	bindOnce.Do(func() {
		if err := bindings.Load(); err != nil {
			return
		}
		lib := bindings.LibAVUtil()
		if lib == 0 {
			return
		}
		purego.RegisterLibFunc(&avGetBytesPerSample, lib, "av_get_bytes_per_sample")
		purego.RegisterLibFunc(&avGetSampleFmtName, lib, "av_get_sample_fmt_name")
		purego.RegisterLibFunc(&avChannelLayoutDefault, lib, "av_channel_layout_default")
	})
}

func bytesPerSample(format int32) int32 {
	bindLib()
	if avGetBytesPerSample == nil {
		return 0
	}
	if n := avGetBytesPerSample(format); n > 0 {
		return n
	}
	return 0
}

// isPlanar asks libavutil for the sample format name instead of trusting enum
// arithmetic: packed formats and their planar twins differ only by the trailing
// 'p', and the enum keeps growing (s64 is packed, s64p is planar).
func isPlanar(format int32) bool {
	bindLib()
	if avGetSampleFmtName != nil {
		if name := avGetSampleFmtName(format); name != nil {
			var last byte
			for i := 0; i < 32; i++ {
				c := *(*byte)(unsafe.Add(name, uintptr(i)))
				if c == 0 {
					break
				}
				last = c
			}
			return last == 'p'
		}
	}
	return format >= 5 && format != 10
}
