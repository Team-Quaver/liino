//go:build windows && (amd64 || arm64)

package winrt

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/runtime/win32"
	rwinrt "github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/winrt"

	"github.com/team-quaver/liino/decode"
	"github.com/team-quaver/liino/pcm"
)

const (
	// pcmRate is the graph's sample rate. Liino's canonical output is 48 kHz;
	// AudioGraph resamples the source for us.
	pcmRate = 48000

	// audioRenderCategoryMedia is AudioRenderCategory.Media.
	audioRenderCategoryMedia = 11

	// audioBufferAccessRead is AudioBufferAccessMode.Read.
	audioBufferAccessRead = 0

	// floatSubtype is MediaEncodingSubtypes.Float
	// (KSDATAFORMAT_SUBTYPE_IEEE_FLOAT).
	floatSubtype = "00000003-0000-0010-8000-00aa00389b71"
)

const (
	// pumpInterval bounds how often the worker asks the graph for new samples.
	// AudioGraph produces in real time, so this only sets the click size.
	pumpInterval = 10 * time.Millisecond

	// framesPerTick caps how many GetFrame calls one pump pass drains. Each
	// call returns a bounded slice of the audio produced since the last call.
	framesPerTick = 4

	// chunkQueue is how much decoded audio the backend may hold (chunks of
	// pcm.ChunkFrames). It also caps how far the real-time graph may run ahead
	// of playback while the pipeline is not consuming.
	chunkQueue = 24

	// lowWater is the queue length at which a stopped graph is restarted.
	lowWater = 6

	// stallLimit is how many empty pump passes (3 s) without progress mark the
	// end of a source whose duration the container did not report.
	stallLimit = 300

	// endStallTicks is the shorter wait (300 ms) that ends a source whose
	// position is already within endSlackFrames of the reported duration. The
	// decoded frame count can trail the container's duration by a few samples;
	// without this the track would hang for the full stallLimit at every end.
	endStallTicks = 30

	// endSlackFrames is how close to the reported end counts as "at the end".
	endSlackFrames = pcmRate / 5 // 200 ms
)

// job is a unit of work executed on the graph's apartment thread.
type job struct {
	fn   func()
	done chan struct{}
}

// stream is one MediaSource/AudioGraph session.
//
// Every COM object, and every call to it, belongs to the worker goroutine
// started by Open: AudioGraph is an apartment-affine API and the graph must be
// created, pumped and torn down on one thread. Read/Seek/Close talk to that
// goroutine, so the fields below the mutex are owned by it alone.
type stream struct {
	uri   string
	live  bool
	codec string

	format pcm.Format
	meta   decode.Meta

	chunks chan pcm.Chunk
	cmds   chan job
	quit   chan struct{}
	done   chan struct{}
	ready  chan error

	mu      sync.Mutex
	closed  bool
	readErr error

	graph     *iAudioGraph
	input     *iMediaSourceAudioInputNode
	audioIn   *iAudioInputNode
	audioNode *iAudioNode
	frameOut  *iAudioFrameOutputNode
	frameNode *iAudioNode
	deviceOut *iAudioDeviceOutputNode

	durationFrames int64 // 0 when the container did not report a duration
	queuedFrames   int64 // frames handed to the chunk queue since Open or Seek
	seekFrames     int64 // source position of the first frame after Open or Seek
	pending        []float32

	running    bool
	eof        bool
	finished   bool
	stallTicks int
}

func newStream(uri, codec string, live bool) *stream {
	return &stream{
		uri:    uri,
		live:   live,
		codec:  codec,
		chunks: make(chan pcm.Chunk, chunkQueue),
		cmds:   make(chan job),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
		ready:  make(chan error, 1),
	}
}

// openWorker initializes the apartment and opens the graph, then serves the
// stream. A thread that already belongs to the host's STA is abandoned and the
// whole attempt starts over on a fresh one (see com.go).
func (s *stream) openWorker(ctx context.Context, attempt int) {
	runtime.LockOSThread()
	if err := rwinrt.RoInitialize(rwinrt.RO_INIT_MULTITHREADED); err != nil {
		runtime.UnlockOSThread()
		if isChangedMode(err) && attempt < mtaInitAttempts {
			go func() {
				time.Sleep(time.Millisecond)
				s.openWorker(ctx, attempt+1)
			}()
			return
		}
		s.ready <- fmt.Errorf("%w: RoInitialize: %v", decode.ErrUnavailable, err)
		return
	}
	defer func() {
		rwinrt.RoUninitialize()
		runtime.UnlockOSThread()
	}()

	openErr := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("winrt: open: %v", r)
			}
		}()
		return s.open(ctx)
	}()
	if openErr != nil {
		s.releaseGraph()
		s.ready <- openErr
		return
	}
	s.ready <- nil
	// A panic here (usually a bad COM call) must fail this stream instead of
	// crashing the host: the worker owns the apartment, so shutdown still has
	// to run on this thread.
	func() {
		defer func() {
			if r := recover(); r != nil {
				s.fail(fmt.Errorf("winrt: worker: %v", r))
			}
		}()
		s.loop()
	}()
	s.shutdown()
}

// open builds the graph. Multichannel sources are reopened at their own channel
// count so AudioGraph does not fold the layout; if that fails, stereo is kept
// so the track still plays.
func (s *stream) open(ctx context.Context) error {
	srcCh, err := s.openGraph(ctx, pcm.DefaultChannels)
	if err != nil {
		return err
	}
	if srcCh > 0 && srcCh != pcm.DefaultChannels {
		s.releaseGraph()
		if _, err := s.openGraph(ctx, srcCh); err != nil {
			s.releaseGraph()
			if _, err := s.openGraph(ctx, pcm.DefaultChannels); err != nil {
				return err
			}
		}
	}
	return nil
}

// openGraph creates one complete graph for ch output channels and returns the
// channel count the media source reports.
func (s *stream) openGraph(ctx context.Context, ch int) (int, error) {
	if ch <= 0 {
		ch = pcm.DefaultChannels
	}

	settingsFactory, err := activationFactory("Windows.Media.Audio.AudioGraphSettings", &iidAudioGraphSettingsFactory)
	if err != nil {
		return 0, fmt.Errorf("%w: AudioGraphSettings: %v", decode.ErrUnavailable, err)
	}
	var settings *iAudioGraphSettings
	err = as[iAudioGraphSettingsFactory](settingsFactory).create(audioRenderCategoryMedia, &settings)
	settingsFactory.Release()
	if err != nil {
		return 0, fmt.Errorf("%w: audio graph settings: %v", decode.ErrUnavailable, err)
	}
	defer settings.Release()

	encStatics, err := activationFactory("Windows.Media.MediaProperties.AudioEncodingProperties", &iidAudioEncodingPropertiesStatics)
	if err != nil {
		return 0, fmt.Errorf("%w: AudioEncodingProperties: %v", decode.ErrUnavailable, err)
	}
	var enc *iAudioEncodingProperties
	err = as[iAudioEncodingPropertiesStatics](encStatics).createPcm(pcmRate, uint32(ch), 24, &enc)
	encStatics.Release()
	if err != nil {
		return 0, fmt.Errorf("%w: pcm encoding: %v", decode.ErrUnavailable, err)
	}
	defer enc.Release()
	if err := settings.setEncodingProperties(enc); err != nil {
		return 0, fmt.Errorf("%w: graph encoding: %v", decode.ErrUnavailable, err)
	}

	graphStatics, err := activationFactory("Windows.Media.Audio.AudioGraph", &iidAudioGraphStatics)
	if err != nil {
		return 0, fmt.Errorf("%w: AudioGraph: %v", decode.ErrUnavailable, err)
	}
	var op *iAsyncOperation
	err = as[iAudioGraphStatics](graphStatics).createAsync(settings, &op)
	graphStatics.Release()
	if err != nil {
		return 0, fmt.Errorf("%w: create audio graph: %v", decode.ErrUnavailable, err)
	}
	var gres *iCreateAudioGraphResult
	if err := awaitOp(ctx, op, unsafe.Pointer(&gres)); err != nil {
		return 0, fmt.Errorf("%w: create audio graph: %v", decode.ErrUnavailable, err)
	}
	defer gres.Release()
	if st, _ := gres.status(); st != statusSuccess {
		return 0, fmt.Errorf("%w: audio graph creation status %d", decode.ErrUnavailable, st)
	}
	if err := gres.graph(&s.graph); err != nil {
		return 0, fmt.Errorf("%w: audio graph: %v", decode.ErrUnavailable, err)
	}

	// A device output node drives AudioGraph's real-time clock. It is left
	// unconnected: creating it is what makes the graph process quanta at
	// playback speed. Without it the graph runs unclocked and decodes as fast
	// as the CPU allows, which makes the buffer unbounded. Silence comes for
	// free because no node feeds it, and this avoids the gain argument of
	// AddOutgoingConnection, which the Windows x64 ABI passes in an XMM
	// register that Go's SyscallN cannot fill.
	var dop *iAsyncOperation
	if err := s.graph.createDeviceOutputNodeAsync(&dop); err != nil {
		return 0, fmt.Errorf("%w: output device: %v", decode.ErrUnsupported, err)
	}
	var dres *iCreateAudioDeviceOutputNodeResult
	if err := awaitOp(ctx, dop, unsafe.Pointer(&dres)); err != nil {
		return 0, fmt.Errorf("%w: output device: %v", decode.ErrUnsupported, err)
	}
	defer dres.Release()
	if st, _ := dres.status(); st != statusSuccess {
		return 0, fmt.Errorf("%w: no audio output device (status %d)", decode.ErrUnsupported, st)
	}
	if err := dres.deviceOutputNode(&s.deviceOut); err != nil {
		return 0, fmt.Errorf("%w: output device: %v", decode.ErrUnsupported, err)
	}

	uriFactory, err := activationFactory("Windows.Foundation.Uri", &iidUriRuntimeClassFactory)
	if err != nil {
		return 0, fmt.Errorf("%w: Uri factory: %v", decode.ErrUnavailable, err)
	}
	h, err := hstring(s.uri)
	if err != nil {
		uriFactory.Release()
		return 0, fmt.Errorf("%w: uri: %v", decode.ErrUnsupported, err)
	}
	var uriObj *iUriRuntimeClass
	uerr := as[iUriRuntimeClassFactory](uriFactory).createUri(h, &uriObj)
	freeHString(h)
	uriFactory.Release()
	if uerr != nil {
		return 0, fmt.Errorf("%w: bad source URI %q: %v", decode.ErrUnsupported, s.uri, uerr)
	}
	defer uriObj.Release()

	msStatics, err := activationFactory("Windows.Media.Core.MediaSource", &iidMediaSourceStatics)
	if err != nil {
		return 0, fmt.Errorf("%w: MediaSource: %v", decode.ErrUnavailable, err)
	}
	var src *iMediaSource
	err = as[iMediaSourceStatics](msStatics).createFromUri(uriObj, &src)
	msStatics.Release()
	if err != nil {
		return 0, fmt.Errorf("%w: media source: %v", decode.ErrUnsupported, err)
	}
	defer src.Release()

	g3, err := win32.QueryInterface[iAudioGraph3](s.graph, &iidAudioGraph3)
	if err != nil {
		return 0, fmt.Errorf("%w: AudioGraph3: %v", decode.ErrUnavailable, err)
	}
	var iop *iAsyncOperation
	err = g3.createMediaSourceAudioInputNodeAsync(src, &iop)
	g3.Release()
	if err != nil {
		return 0, fmt.Errorf("%w: input node: %v", decode.ErrUnsupported, err)
	}
	var ires *iCreateMediaSourceInputNodeResult
	if err := awaitOp(ctx, iop, unsafe.Pointer(&ires)); err != nil {
		return 0, fmt.Errorf("%w: open %q: %v", decode.ErrUnsupported, s.uri, err)
	}
	defer ires.Release()
	if st, _ := ires.status(); st != statusSuccess {
		// UnknownFailure here is what a missing demuxer/codec looks like - e.g.
		// Ogg on a machine without Web Media Extensions.
		var ext int32
		if r2, err := win32.QueryInterface[iCreateMediaSourceInputNodeResult2](ires, &iidCreateMediaSourceInputNodeResult2); err == nil {
			_ = r2.extendedError(&ext)
			r2.Release()
		}
		return 0, fmt.Errorf("%w: media source open status %d (extended error 0x%08x, uri %s)",
			decode.ErrUnsupported, st, uint32(ext), s.uri)
	}
	if err := ires.node(&s.input); err != nil {
		return 0, fmt.Errorf("%w: input node: %v", decode.ErrUnsupported, err)
	}

	s.audioIn, err = win32.QueryInterface[iAudioInputNode](s.input, &iidAudioInputNode)
	if err != nil {
		return 0, fmt.Errorf("%w: IAudioInputNode: %v", decode.ErrUnsupported, err)
	}
	s.audioNode, err = win32.QueryInterface[iAudioNode](s.input, &iidAudioNode)
	if err != nil {
		return 0, fmt.Errorf("%w: IAudioNode: %v", decode.ErrUnsupported, err)
	}

	srcCh, srcRate := 0, 0
	var ep *iAudioEncodingProperties
	if err := s.audioNode.encodingProperties(&ep); err == nil && ep != nil {
		if v, err := ep.channelCount(); err == nil && v > 0 {
			srcCh = int(v)
		}
		if v, err := ep.sampleRate(); err == nil && v > 0 {
			srcRate = int(v)
		}
		ep.Release()
	}

	fenc, err := newFloatEncoding(uint32(ch))
	if err != nil {
		return 0, fmt.Errorf("%w: frame format: %v", decode.ErrUnsupported, err)
	}
	defer fenc.Release()
	if err := s.graph.createFrameOutputNodeWithFormat(fenc, &s.frameOut); err != nil {
		return 0, fmt.Errorf("%w: frame output node: %v", decode.ErrUnsupported, err)
	}
	if n, err := win32.QueryInterface[iAudioNode](s.frameOut, &iidAudioNode); err == nil {
		s.frameNode = n
	}

	// The connection takes an IAudioNode*, so pass the interface pointer that
	// QueryInterface returned for the node rather than its frame-output
	// pointer; WinRT may hand out per-interface pointers.
	var frameNode unsafe.Pointer = unsafe.Pointer(s.frameOut)
	if s.frameNode != nil {
		frameNode = unsafe.Pointer(s.frameNode)
	}
	if err := s.audioIn.addOutgoingConnection(frameNode); err != nil {
		return 0, fmt.Errorf("%w: connect frame output: %v", decode.ErrUnsupported, err)
	}

	var durTicks int64
	if !s.live {
		if v, err := s.input.duration(); err == nil {
			durTicks = v
		}
	}

	s.format = pcm.Format{SampleRate: pcmRate, Channels: ch}
	s.durationFrames = ticksToFrames(durTicks)
	s.meta = decode.Meta{
		Codec:      s.codec,
		SampleRate: srcRate,
		Channels:   ch,
		Duration:   ticksToDuration(durTicks),
		Seekable:   !s.live,
		Live:       s.live,
	}
	if s.meta.SampleRate <= 0 {
		s.meta.SampleRate = pcmRate
	}
	if s.live {
		s.meta.Duration = 0
		s.durationFrames = 0
	}
	return srcCh, nil
}

// newFloatEncoding builds the float32 PCM format used for the frame output
// node. AudioGraph accepts a constructed format here even though graph settings
// require CreatePcm.
func newFloatEncoding(ch uint32) (*iAudioEncodingProperties, error) {
	inst, err := activateInstance("Windows.Media.MediaProperties.AudioEncodingProperties")
	if err != nil {
		return nil, err
	}
	defer inst.Release()

	aep, err := win32.QueryInterface[iAudioEncodingProperties](inst, &iidAudioEncodingProperties)
	if err != nil {
		return nil, err
	}
	mep, err := win32.QueryInterface[iMediaEncodingProperties](inst, &iidMediaEncodingProperties)
	if err != nil {
		aep.Release()
		return nil, err
	}
	defer mep.Release()

	if err := aep.setSampleRate(pcmRate); err != nil {
		aep.Release()
		return nil, err
	}
	if err := aep.setChannelCount(ch); err != nil {
		aep.Release()
		return nil, err
	}
	if err := aep.setBitsPerSample(32); err != nil {
		aep.Release()
		return nil, err
	}
	if err := aep.setBitrate(pcmRate * ch * 32); err != nil {
		aep.Release()
		return nil, err
	}
	sub, err := hstring(floatSubtype)
	if err != nil {
		aep.Release()
		return nil, err
	}
	err = mep.setSubtype(sub)
	freeHString(sub)
	if err != nil {
		aep.Release()
		return nil, err
	}
	return aep, nil
}

// loop serves commands and pumps the graph until the stream is closed or fails.
func (s *stream) loop() {
	ticker := time.NewTicker(pumpInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.quit:
			return
		case j := <-s.cmds:
			j.fn()
			close(j.done)
			if s.finished {
				return
			}
		case <-ticker.C:
			s.pump()
			if s.finished {
				return
			}
		}
	}
}

// pump moves audio from the real-time graph into the bounded chunk queue.
// The graph is stopped whenever the queue is full, which is what lets pause
// work: the decoder cannot race ahead of the consumer.
func (s *stream) pump() {
	if s.finished {
		return
	}
	if s.eof {
		s.emitPending(true)
		if len(s.chunks) == 0 {
			s.finished = true
			close(s.chunks)
		}
		return
	}

	if !s.running {
		if len(s.chunks) >= lowWater {
			return
		}
		if err := s.graph.start(); err != nil {
			s.fail(fmt.Errorf("winrt: start graph: %w", err))
			return
		}
		s.running = true
		s.stallTicks = 0
	}

	got := 0
	for i := 0; i < framesPerTick; i++ {
		if len(s.chunks) >= cap(s.chunks) {
			s.pauseGraph()
			break
		}
		data, frames, err := s.readFrame()
		if err != nil {
			s.fail(err)
			return
		}
		if frames == 0 {
			break
		}
		got++
		s.pending = append(s.pending, data...)
		s.emitPending(false)
	}
	// Flush complete chunks even on a pass that read nothing: after the source
	// ends, the tail can still be pending and the queue may have space again.
	s.emitPending(false)

	if s.durationFrames > 0 && s.seekFrames+s.queuedFrames >= s.durationFrames {
		s.eof = true
		if s.running {
			_ = s.graph.stop()
			s.running = false
		}
		s.emitPending(true)
		return
	}
	if s.live {
		return
	}
	// A source with no reported duration ends when the graph keeps producing
	// nothing for a while; a running graph with audio resets the counter.
	if got == 0 {
		s.stallTicks++
		limit := stallLimit
		if s.durationFrames > 0 {
			if remain := s.durationFrames - s.seekFrames - s.queuedFrames; remain >= 0 && remain <= endSlackFrames {
				limit = endStallTicks
			}
		}
		if s.stallTicks > limit {
			s.eof = true
			if s.running {
				_ = s.graph.stop()
				s.running = false
			}
			s.emitPending(true)
		}
	} else {
		s.stallTicks = 0
	}
}

// pauseGraph stops the real-time graph when the queue is full, after taking
// the samples it produced since the last poll. Stopping without that last
// GetFrame drops up to one pump interval of audio, which leaves an audible
// gap (and a phase shift) every time playback resumes.
func (s *stream) pauseGraph() {
	if !s.running {
		return
	}
	for i := 0; i < framesPerTick; i++ {
		data, frames, err := s.readFrame()
		if err != nil || frames == 0 {
			break
		}
		s.pending = append(s.pending, data...)
	}
	_ = s.graph.stop()
	s.running = false
}

// readFrame returns the samples the graph produced since the previous call.
func (s *stream) readFrame() ([]float32, int, error) {
	var frame *iAudioFrame
	if err := s.frameOut.getFrame(&frame); err != nil {
		return nil, 0, fmt.Errorf("winrt: get frame: %w", err)
	}
	if frame == nil {
		return nil, 0, nil
	}
	defer frame.Release()

	var buf *iAudioBuffer
	if err := frame.lockBuffer(audioBufferAccessRead, &buf); err != nil {
		return nil, 0, fmt.Errorf("winrt: lock frame: %w", err)
	}
	if buf == nil {
		return nil, 0, nil
	}
	data, err := readBuffer(buf)
	closeObject(&buf.IInspectable)
	buf.Release()
	if err != nil {
		return nil, 0, err
	}
	ch := s.format.Channels
	if ch <= 0 || len(data) < ch {
		return nil, 0, nil
	}
	return data, len(data) / ch, nil
}

// readBuffer copies the float samples out of a locked AudioBuffer.
func readBuffer(buf *iAudioBuffer) ([]float32, error) {
	n, err := buf.length()
	if err != nil {
		return nil, fmt.Errorf("winrt: buffer length: %w", err)
	}
	if n == 0 {
		return nil, nil
	}
	mb, err := win32.QueryInterface[iMemoryBuffer](buf, &iidMemoryBuffer)
	if err != nil {
		return nil, fmt.Errorf("winrt: buffer memory: %w", err)
	}
	var ref *iMemoryBufferReference
	err = mb.createReference(&ref)
	mb.Release()
	if err != nil {
		return nil, fmt.Errorf("winrt: buffer reference: %w", err)
	}
	defer func() {
		closeObject(&ref.IInspectable)
		ref.Release()
	}()

	mba, err := win32.QueryInterface[rwinrt.IMemoryBufferByteAccess](ref, &rwinrt.IID_IMemoryBufferByteAccess)
	if err != nil {
		return nil, fmt.Errorf("winrt: buffer access: %w", err)
	}
	defer mba.Release()

	var (
		p        *byte
		capacity uint32
	)
	if err := mba.GetBuffer(&p, &capacity); err != nil {
		return nil, fmt.Errorf("winrt: buffer data: %w", err)
	}
	count := int(n) / 4
	if max := int(capacity) / 4; count > max {
		count = max
	}
	if count <= 0 || p == nil {
		return nil, nil
	}
	out := make([]float32, count)
	copy(out, unsafe.Slice((*float32)(unsafe.Pointer(p)), count))
	return out, nil
}

// emitPending moves complete pcm.ChunkFrames chunks (and, when final, the
// remainder) from pending to the queue. It never blocks: the queue's capacity
// is checked first, and only this goroutine sends.
func (s *stream) emitPending(final bool) {
	ch := s.format.Channels
	if ch <= 0 {
		return
	}
	chunkFloats := pcm.ChunkFrames * ch
	for len(s.chunks) < cap(s.chunks) {
		n := len(s.pending)
		if n >= chunkFloats {
			d := make([]float32, chunkFloats)
			copy(d, s.pending[:chunkFloats])
			s.pending = s.pending[chunkFloats:]
			s.chunks <- pcm.Chunk{Data: d, Frames: pcm.ChunkFrames, Format: s.format}
			s.queuedFrames += pcm.ChunkFrames
			continue
		}
		if final && n >= ch {
			frames := n / ch
			d := make([]float32, frames*ch)
			copy(d, s.pending[:frames*ch])
			s.pending = s.pending[:0]
			s.chunks <- pcm.Chunk{Data: d, Frames: frames, Format: s.format}
			s.queuedFrames += int64(frames)
		}
		return
	}
}

// fail records a fatal error, stops the graph and closes the queue so Read
// drains what is left and then returns the error.
func (s *stream) fail(err error) {
	s.mu.Lock()
	if s.readErr == nil {
		s.readErr = err
	}
	s.mu.Unlock()
	if s.running {
		_ = s.graph.stop()
		s.running = false
	}
	if !s.finished {
		s.finished = true
		close(s.chunks)
	}
}

// shutdown releases the graph and wakes Read.
func (s *stream) shutdown() {
	if s.running {
		_ = s.graph.stop()
		s.running = false
	}
	s.releaseGraph()
	if !s.finished {
		s.finished = true
		close(s.chunks)
	}
	close(s.done)
}

// releaseGraph drops every COM reference this session owns, newest first.
func (s *stream) releaseGraph() {
	if s.frameNode != nil {
		s.frameNode.Release()
		s.frameNode = nil
	}
	if s.frameOut != nil {
		s.frameOut.Release()
		s.frameOut = nil
	}
	if s.audioNode != nil {
		s.audioNode.Release()
		s.audioNode = nil
	}
	if s.audioIn != nil {
		s.audioIn.Release()
		s.audioIn = nil
	}
	if s.input != nil {
		s.input.Release()
		s.input = nil
	}
	if s.deviceOut != nil {
		s.deviceOut.Release()
		s.deviceOut = nil
	}
	if s.graph != nil {
		s.graph.Release()
		s.graph = nil
	}
}

// do runs fn on the graph thread and waits for it.
func (s *stream) do(fn func()) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return decode.ErrNotOpen
	}
	s.mu.Unlock()

	done := make(chan struct{})
	select {
	case s.cmds <- job{fn: fn, done: done}:
	case <-s.done:
		return decode.ErrNotOpen
	}
	select {
	case <-done:
		return nil
	case <-s.done:
		return decode.ErrNotOpen
	}
}

func (s *stream) Format() pcm.Format { return s.format }
func (s *stream) Meta() decode.Meta  { return s.meta }

func (s *stream) Read() (pcm.Chunk, error) {
	chunk, ok := <-s.chunks
	if ok {
		return chunk, nil
	}
	s.mu.Lock()
	err := s.readErr
	s.mu.Unlock()
	if err != nil {
		return pcm.Chunk{}, err
	}
	return pcm.Chunk{}, io.EOF
}

func (s *stream) Seek(t time.Duration) (time.Duration, error) {
	if !s.meta.Seekable {
		return 0, decode.ErrSeekUnsupported
	}
	if t < 0 {
		t = 0
	}
	var (
		at  time.Duration
		err error
	)
	if jerr := s.do(func() { at, err = s.seek(t) }); jerr != nil {
		return 0, jerr
	}
	return at, err
}

// seek runs on the graph thread. Queued audio from the old position is thrown
// away; the pipeline has already reset its own ring.
func (s *stream) seek(t time.Duration) (time.Duration, error) {
	if s.finished || s.graph == nil {
		return 0, decode.ErrNotOpen
	}
	for {
		select {
		case <-s.chunks:
			continue
		default:
		}
		break
	}
	s.pending = s.pending[:0]
	s.queuedFrames = 0
	s.eof = false
	s.stallTicks = 0
	// Keep the graph running across the position change: restarting a parked
	// AudioGraph resets the media source to 0 and silently discards the Seek.
	// Start it first when the pump had parked it because the queue was full.
	if !s.running {
		if err := s.graph.start(); err != nil {
			return 0, fmt.Errorf("winrt: seek start graph: %w", err)
		}
		s.running = true
	}
	ticks := int64(t / (100 * time.Nanosecond))
	if err := s.input.seek(ticks); err != nil {
		return 0, fmt.Errorf("winrt: seek: %w", err)
	}
	// The container may land on an index point rather than the exact target.
	// Track the actual origin so EOF accounting counts down from the seek
	// target, not from 0 (otherwise the post-seek tail is treated as a full
	// track and the clock runs past the end).
	s.seekFrames = ticksToFrames(ticks)
	if pos, err := s.input.position(); err == nil {
		s.seekFrames = ticksToFrames(pos)
	}
	if s.frameNode != nil {
		// Drop frames the output node buffered at the previous position. The
		// reset must follow Seek: a running graph can emit one more quantum
		// between the position change and this point.
		_ = s.frameNode.reset()
	}
	return t, nil
}

func (s *stream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	close(s.quit)
	<-s.done
	return nil
}

// ticksToFrames converts a Windows TimeSpan (100 ns ticks) to frames at pcmRate.
// The split avoids overflowing ticks*48000 for long sources.
func ticksToFrames(ticks int64) int64 {
	if ticks <= 0 {
		return 0
	}
	const perSecond = int64(10_000_000)
	return (ticks/perSecond)*pcmRate + (ticks%perSecond)*pcmRate/perSecond
}

// ticksToDuration converts 100 ns ticks to a time.Duration, refusing values
// that would overflow the conversion.
func ticksToDuration(ticks int64) time.Duration {
	if ticks <= 0 || ticks > int64(maxDuration/100) {
		return 0
	}
	return time.Duration(ticks*100) * time.Nanosecond
}

const maxDuration = time.Duration(1<<63 - 1)
