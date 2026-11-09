// Package liino is Quaver Music's native audio backend: pure Go (CGO_ENABLED=0)
// from demux to speaker, built to be loaded as a dynamic library by Electron.
//
// The pipeline is fixed and boring on purpose:
//
//	decode (ffgo / libav.js)  ->  canonical PCM 48kHz stereo float32
//	    ->  dsp.Pipeline (EQ, effects chain, gain, limiter, fades, meter)
//	        ->  pcm.Pipe (bounded ring, backpressure)
//	            ->  output driver (WASAPI / Core Audio / PipeWire)
//
// Everything runs inside the host process. There is no mpv child, no named pipe
// to a helper, and no WASM sandbox: on Windows the render session is owned by
// Quaver itself in shared mode, which is what lets Discord, OBS and Kook capture
// the app's audio and nothing else.
//
// A Player owns one output session. Create one per app, drive it with Open /
// Play / Pause / Seek, and read Events for state transitions. Generations ride
// on every event so a late callback from the previous song can never move the
// UI.
package liino

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/team-quaver/liino/decode"
	"github.com/team-quaver/liino/dsp"
	"github.com/team-quaver/liino/output"
	"github.com/team-quaver/liino/pcm"

	// Backends register themselves; the stubs are no-ops on other platforms.
	_ "github.com/team-quaver/liino/decode/avf"
	_ "github.com/team-quaver/liino/decode/native"
	_ "github.com/team-quaver/liino/decode/ogg"
	_ "github.com/team-quaver/liino/decode/wasm"
	_ "github.com/team-quaver/liino/decode/winrt"
	_ "github.com/team-quaver/liino/decode/wmf"
	_ "github.com/team-quaver/liino/output/pipewire"
	_ "github.com/team-quaver/liino/output/system"
)

// Version is the library version, surfaced to Node over the FFI boundary.
const Version = "0.1.0"

// Defaults for a desktop music player: 5 s of buffer, 50 ms preroll, 5 ms
// periods, and a device sanity check every 2 s.
const (
	DefaultRingFrames      = pcm.DefaultSampleRate * 5
	DefaultPrerollFrames   = pcm.ChunkFrames
	DefaultPeriodFrames    = output.DefaultPeriodFrames
	DefaultMonitorInterval = 40 * time.Millisecond
	DefaultDevicePolling   = 2 * time.Second
	DefaultFadeOut         = 120 * time.Millisecond
	DefaultBufferLowFrames = pcm.ChunkFrames / 2
)

// Config sets up a Player. The zero value is valid.
type Config struct {
	// Decoder selects a decode backend name ("wmf", "avf", "ffgo", "libavjs").
	// Empty means automatic selection per source: platform backends first, then
	// ffgo/libav.js when the OS cannot decode the container or codec.
	Decoder string
	// Driver selects an output driver ("wasapi", "coreaudio", "pipewire",
	// "null"); empty picks the platform default.
	Driver string
	// DeviceID is the endpoint to open. Empty means the system default.
	DeviceID string

	// AppName and StreamName are what the OS mixer shows. Keep AppName equal to
	// "Quaver Music" so per-app capture keeps finding the right process.
	AppName    string
	StreamName string

	RingFrames      int
	PrerollFrames   int
	PeriodFrames    int
	MonitorInterval time.Duration
	DevicePolling   time.Duration
	FadeOut         time.Duration

	// Volume is the initial linear master gain; 0 means 1.0.
	Volume float64
	// EQ installs bands before the first frame is processed.
	EQ []dsp.Config
	// Limiter, when set, clamps output above ThresholdDB.
	Limiter        bool
	LimiterDB      float64
	LimiterAttack  float64 // ms
	LimiterRelease float64 // ms

	// Log receives diagnostics; OnError receives asynchronous failures.
	Log     func(string)
	OnError func(error)
}

func (c *Config) withDefaults() error {
	// Decoder stays empty for automatic, per-source selection. Pinning it to the
	// process default here would hide the platform decoder's fallback: a WMF
	// machine would never hand an Ogg file to ffgo.
	if _, ok := decode.Lookup(c.Decoder); c.Decoder != "" && !ok {
		return fmt.Errorf("%w: decoder %q", ErrInvalidConfig, c.Decoder)
	}
	if c.Driver != "" {
		if _, ok := output.Lookup(c.Driver); !ok {
			return fmt.Errorf("%w: output driver %q", ErrInvalidConfig, c.Driver)
		}
	}
	if c.RingFrames <= 0 {
		c.RingFrames = DefaultRingFrames
	}
	if c.PrerollFrames <= 0 {
		c.PrerollFrames = DefaultPrerollFrames
	}
	if c.PrerollFrames > c.RingFrames {
		c.PrerollFrames = c.RingFrames / 4
	}
	if c.PeriodFrames <= 0 {
		c.PeriodFrames = DefaultPeriodFrames
	}
	if c.MonitorInterval <= 0 {
		c.MonitorInterval = DefaultMonitorInterval
	}
	if c.DevicePolling <= 0 {
		c.DevicePolling = DefaultDevicePolling
	}
	if c.Volume == 0 {
		c.Volume = 1
	}
	if c.AppName == "" {
		c.AppName = "Quaver Music"
	}
	if c.LimiterDB == 0 {
		c.LimiterDB = -1
	}
	if c.LimiterAttack == 0 {
		c.LimiterAttack = 3
	}
	if c.LimiterRelease == 0 {
		c.LimiterRelease = 120
	}
	return nil
}

// Player is one audio session: a decoder, a DSP chain, a buffer and an output.
type Player struct {
	cfg Config

	// pipe is replaced when a stream's channel count changes. The audio callback
	// must never take a lock that a UI-thread FFI call can hold, so the pointer
	// is atomic rather than guarded by p.mu.
	pipe atomic.Pointer[pcm.Pipe]
	// outChannels is the channel count the open endpoint was negotiated for.
	// Read adapts the pipe's source layout to it (downmix/upmix) on the fly.
	outChannels atomic.Int32
	// readBuf is only touched by the output callback; it stages a multichannel
	// read before folding it into the endpoint's layout.
	readBuf []float32
	// outGain is the consumer-side master gain. It runs after the ring so a
	// volume move is heard immediately instead of waiting for seconds of
	// already-processed audio to drain.
	outGain *dsp.AtomicGain

	chain *dsp.Pipeline
	hub   *eventBus
	clock *clock

	mu sync.Mutex
	// openMu serialises device opens without holding mu: PipeWire/WASAPI stream
	// creation may block for an unbounded time, and FFI callers (clock/snapshot)
	// must never wait on it from the host's UI thread.
	openMu      sync.Mutex
	state       State
	src         decode.Source
	meta        decode.Meta
	stream      decode.Stream
	decoderName string
	out         output.Output
	driver      output.Driver
	device      output.Device
	err         error
	base        time.Duration
	eof         bool
	wantPlay    bool
	// playIntent is what the user asked for: Play sets it, Pause/Stop/Open clear
	// or set it. wantPlay is narrower — "the device may run right now" — and the
	// engine clears it whenever the machine parks itself (end of file, failure,
	// a paused session being torn down). Rebuilds must follow the intent, not the
	// parked flag: clearing both at end of file is what made a seek after the
	// last frame reopen a pipeline that nobody was allowed to start, leaving the
	// app silent with the progress bar frozen.
	playIntent bool
	primed     bool
	// seekRebuilds is the remaining decoder-rebuild budget for failed seeks in
	// the current load (see rebuildAfterSeek / takeSeekRebuild).
	seekRebuilds int
	// loopAlive reports whether a decode goroutine owns this generation. A
	// seek that arrives after it retired (end of file, failure) must restart
	// the source itself: nobody else would read the queued target.
	loopAlive bool
	pending   func() // deferred action after a fade, e.g. stop
	volume    float64
	muted     bool

	pulled atomic.Uint64 // frames handed to the device since the last seek
	// bufferAt records pulled when the pipeline entered StateBuffering. A
	// real-time decoder (WinRT AudioGraph) does not fill the pipe to half the
	// preroll depth, so recovery must also accept frames actually reaching the
	// device; otherwise seek leaves the UI in a permanent buffering state while
	// audio keeps playing.
	bufferAt uint64
	// starting is the single-flight flag for the asynchronous device start
	// (see requestDeviceStart). Without it the monitor would queue one blocked
	// session open after another behind a sound service that never answers.
	starting atomic.Bool
	decoded  atomic.Uint64 // frames produced by the decoder since the last seek
	gen      generations
	quit     chan struct{}
	wake     chan struct{}
	seekReq  chan seekRequest
	closed   bool
	decWG    sync.WaitGroup
	monWG    sync.WaitGroup
}

// New creates a player. It does not touch an audio device until Play is called,
// so constructing one on a headless machine is safe.
func New(cfg Config) (*Player, error) {
	if err := cfg.withDefaults(); err != nil {
		return nil, err
	}
	p := &Player{
		cfg:     cfg,
		chain:   dsp.NewPipeline(pcm.DefaultSampleRate, pcm.DefaultChannels),
		outGain: dsp.NewAtomicGain(pcm.DefaultSampleRate),
		hub:     newEventBus(),
		clock:   newClock(pcm.DefaultSampleRate),
		quit:    make(chan struct{}),
		wake:    make(chan struct{}, 1),
		seekReq: make(chan seekRequest, 1),
		volume:  cfg.Volume,
	}
	p.pipe.Store(pcm.NewPipe(cfg.RingFrames, pcm.DefaultFormat()))
	p.outChannels.Store(int32(pcm.DefaultChannels))
	// User volume lives on the consumer side; the producer chain stays at
	// unity (EQ bands still run there). Preamp is folded into the gain value by
	// the host, so it lands here too.
	p.outGain.SetLinear(cfg.Volume)
	p.outGain.Snap()
	if len(cfg.EQ) > 0 {
		if err := p.chain.SetBands(cfg.EQ); err != nil {
			return nil, fmt.Errorf("%w: eq: %v", ErrInvalidConfig, err)
		}
	}
	if cfg.Limiter {
		p.chain.SetLimiter(true, cfg.LimiterDB, cfg.LimiterAttack, cfg.LimiterRelease)
	}
	p.chain.SetBypass(false)
	p.monWG.Add(1)
	go p.monitor()
	return p, nil
}

// OpenOptions tunes one load.
type OpenOptions struct {
	// StartAt begins decoding at an offset, used for resume and range seeks
	// before playback starts.
	StartAt time.Duration
	// AutoPlay starts the device as soon as the preroll buffer is full.
	AutoPlay bool
}

// Open loads a source. It returns as soon as the request is queued: metadata
// arrives as Event{Kind: KindLoaded} and failures as KindFailed, both stamped
// with the current generation, so a newer Open always wins over an older one.
func (p *Player) Open(ctx context.Context, src decode.Source, opts OpenOptions) error {
	if src.URI == "" && src.Reader == nil {
		return fmt.Errorf("%w: empty source", ErrInvalidConfig)
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	p.gen.nextTrack()
	gen := p.gen.nextLoad()
	p.gen.nextSeek()
	p.src = src
	p.meta = decode.Meta{}
	p.err = nil
	p.state = StateLoading
	p.eof = false
	p.primed = false
	p.wantPlay = opts.AutoPlay
	p.playIntent = opts.AutoPlay
	p.seekRebuilds = seekRebuildBudget
	p.loopAlive = true
	p.dropQueuedSeek() // a target queued for the previous track must not move this one
	p.pending = nil
	p.pulled.Store(0)
	p.bufferAt = 0
	p.decoded.Store(0)
	p.base = maxDuration(opts.StartAt, 0)
	p.clock = newClock(pcm.DefaultSampleRate)
	p.clock.base = p.base
	// A stale producer is cut off by the token swap and notices the generation
	// change, but it no longer owns the stream: this load does. Take the old
	// stream away and close it in the background, or every skip leaks a
	// decoder and its HTTP connection (the abandoned Read can be parked for up
	// to rw_timeout, so Close must not run on this path).
	oldStream := p.stream
	p.stream = nil
	p.pipe.Load().Swap(gen)
	p.chain.Reset()
	p.nudge()
	p.mu.Unlock()

	if oldStream != nil {
		p.decWG.Add(1)
		go func() {
			defer p.decWG.Done()
			_ = oldStream.Close()
		}()
	}
	p.decWG.Add(1)
	go p.decodeLoop(ctx, gen, src, opts.StartAt)
	return nil
}

// Play opens the device (if needed) and starts or resumes playback, waiting in
// the background for the preroll buffer when the pipeline is still cold.
func (p *Player) Play() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	if p.state == StateIdle || p.state == StateStopped || p.state == StateFailed {
		if p.src.URI == "" && p.src.Reader == nil {
			p.mu.Unlock()
			return ErrNoMedia
		}
		gen, src := p.gen.curLoad(), p.src
		p.state = StateLoading
		p.seekRebuilds = seekRebuildBudget
		p.loopAlive = true
		p.mu.Unlock()
		p.decWG.Add(1)
		go p.decodeLoop(context.Background(), gen, src, p.base)
		p.mu.Lock()
	}
	p.wantPlay = true
	p.playIntent = true
	switch p.state {
	case StateEnded:
		// Replay after end of file: reopen from the top.
		p.mu.Unlock()
		return p.reopen(time.Duration(0), true)
	case StatePaused:
		out := p.out
		p.mu.Unlock()
		if out == nil {
			// A device switch that failed leaves the session gone while the state
			// still says paused. Opening an endpoint can block on the OS mixer and
			// Play() is called straight from the host's FFI thread, so hand the
			// start back to the monitor — wantPlay is already set — instead of
			// opening it here or recursing into Play() until the stack dies.
			p.mu.Lock()
			if !p.closed && p.state == StatePaused {
				p.state = StateReady
			}
			p.mu.Unlock()
			p.nudge()
			return nil
		}
		if err := out.Resume(); err != nil {
			p.fail(err)
			return err
		}
		p.setState(StatePlaying, KindPlaying)
		return nil
	}
	p.mu.Unlock()
	p.nudge()
	return nil
}

// Pause keeps the device session alive and emits silence, so resuming is
// instant and gap-free.
func (p *Player) Pause() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	out := p.out
	state := p.state
	// Pause is a user decision, so it owns both flags. Without clearing
	// playIntent a later rebuild (unseekable source, failed seek) would answer
	// "resume?" with yes and start making sound on a track the user paused.
	p.playIntent = false
	p.wantPlay = false
	p.mu.Unlock()
	if state == StatePaused {
		return nil
	}
	if out == nil {
		return nil
	}
	if err := out.Pause(); err != nil {
		p.fail(err)
		return err
	}
	p.setState(StatePaused, KindPaused)
	return nil
}

// TrackGeneration reports which Open owns the player. It only advances on a
// real track change, not on an internal decoder rebuild (a failed seek or read
// recovery keeps the same track). Detached FFI commands capture it before
// returning to the host, so a command issued for an old track can recognise a
// newer Open that landed while it was queued.
func (p *Player) TrackGeneration() uint64 {
	return p.gen.curTrack()
}

// Stop ends playback. With a configured fade it first ramps the tail out so the
// device never hears a hard cut.
func (p *Player) Stop() error {
	return p.StopIf(0)
}

// StopIf is Stop guarded by the track generation the caller observed. Pass 0 to
// stop whatever is current. The C API detaches stop (it may wait on a network
// read or a device session), which means a stop issued for the previous track
// can otherwise execute long after the next Open and tear down the new stream:
// the renderer then waits forever for a loaded event that can no longer come.
func (p *Player) StopIf(gen uint64) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	if gen != 0 && gen != p.gen.curTrack() {
		p.mu.Unlock()
		return nil
	}
	p.wantPlay = false
	fade := p.cfg.FadeOut
	p.playIntent = false
	playing := p.state == StatePlaying || p.state == StateBuffering || p.state == StateReady
	pending := p.pending
	p.pending = nil
	p.mu.Unlock()
	if pending != nil {
		pending()
	}
	if !playing {
		p.hardStop()
		return nil
	}
	if fade <= 0 {
		p.hardStop()
		return nil
	}
	p.mu.Lock()
	p.chain.StartFade(dsp.FadeOut, float64(fade/time.Millisecond), dsp.CurveEqualPower)
	p.pending = func() { p.hardStop() }
	p.mu.Unlock()
	p.nudge()
	return nil
}

func (p *Player) hardStop() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	gen := p.gen.nextLoad()
	p.pending = nil
	p.wantPlay = false
	p.playIntent = false
	p.eof = false
	p.primed = false
	p.dropQueuedSeek()
	p.loopAlive = false
	p.state = StateStopped
	p.clock.base = 0
	p.pulled.Store(0)
	p.bufferAt = 0
	p.pipe.Load().Interrupt()
	p.pipe.Load().Reset()
	p.chain.Reset()
	stream := p.stream
	p.stream = nil
	out := p.out
	p.mu.Unlock()

	if stream != nil {
		_ = stream.Close()
	}
	// Closing the stream can wait on a network read (up to rw_timeout). If a
	// newer Open landed meanwhile it owns the same output object, so stopping
	// it here would silence the new track; leave the session to the new lease.
	p.mu.Lock()
	stale := p.closed || p.gen.curLoad() != gen
	p.mu.Unlock()
	stoppedOutput := false
	if !stale && out != nil {
		_ = out.Stop()
		stoppedOutput = true
	}
	if !p.publishFrom(gen, Event{Kind: KindClosed, Name: "stopped"}) && stoppedOutput {
		p.resumeAfterSupersededStop(gen)
	}
}

// resumeAfterSupersededStop repairs the one race generation checks cannot:
// Stop captured the output before a newer Open landed, so out.Stop() may have
// muted the share now owned by that load. Put a still-Playing state back to
// Ready so the monitor starts the device again (a stalled session must not look
// like playback to the UI).
func (p *Player) resumeAfterSupersededStop(gen uint64) {
	p.mu.Lock()
	if p.closed || p.gen.curLoad() == gen {
		p.mu.Unlock()
		return
	}
	restart := p.wantPlay && (p.state == StateLoading || p.state == StateReady || p.state == StatePlaying)
	if restart && p.state == StatePlaying {
		p.state = StateReady
	}
	p.mu.Unlock()
	if restart {
		p.nudge()
	}
}

// Seek jumps to a position. The device keeps running: the buffer is dropped,
// the decoder is repositioned, and the shortfall shows up as a short buffering
// window instead of a time jump.
func (p *Player) Seek(d time.Duration) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	if d < 0 {
		d = 0
	}
	if p.meta.Duration > 0 && d > p.meta.Duration {
		d = p.meta.Duration
	}
	seekGen := p.gen.nextSeek()
	p.clock.base = d
	p.pulled.Store(0)
	p.bufferAt = 0
	p.eof = false
	p.chain.Reset()
	p.pipe.Load().Reset()
	p.pipe.Load().Interrupt()
	stream := p.stream
	meta := p.meta
	alive := p.loopAlive
	if alive {
		// Queue under the same lock that retires the loop: either the decoder
		// sees this target, or it already marked itself dead and we restart
		// below. A seek must never be queued to a goroutine that has exited.
		p.enqueueSeek(seekRequest{at: d, gen: seekGen})
	}
	p.mu.Unlock()

	if stream == nil {
		if p.cfg.FadeOut > 0 {
			p.mu.Lock()
			p.pending = nil
			p.mu.Unlock()
		}
		// Nothing decoded yet: the position is remembered for the next load.
		return nil
	}
	if !meta.Seekable || !alive {
		// No live decoder to serve the target (unseekable source, or the loop
		// retired at EOF/a failure): rebuild the pipeline at d.
		return p.reopen(d, p.resumeIntent())
	}
	// Run the seek on the decode goroutine (applyPendingSeek), never here.
	//
	// Performing it in a goroutine per UI event used to queue one network seek
	// after another behind the stream lock: dragging the progress bar piled up
	// dozens of stale targets, each doing a full HTTP range search, and the
	// decoder produced nothing until the whole queue drained — the "seek
	// freezes playback / stutters" regression. The channel holds only the
	// newest target, so a drag costs at most one in-flight seek plus one
	// queued replacement.
	p.nudge()
	return nil
}

// seekRequest is one pending position. gen identifies the Seek that produced
// it so a target superseded while another seek is in flight can be dropped.
type seekRequest struct {
	at  time.Duration
	gen uint64
}

// enqueueSeek replaces the queued target with req. It never blocks (the UI
// thread must not wait on the decoder) and only the newest target survives.
func (p *Player) enqueueSeek(req seekRequest) {
	select {
	case <-p.seekReq:
	default:
	}
	select {
	case p.seekReq <- req:
	default:
	}
}

// requeueSeek puts a target back only when the slot is free: a newer target
// already queued wins (the old one is stale by definition).
func (p *Player) requeueSeek(req seekRequest) {
	select {
	case p.seekReq <- req:
	default:
	}
}

// takeQueuedSeek pops the queued target, if any.
func (p *Player) takeQueuedSeek() (seekRequest, bool) {
	select {
	case req := <-p.seekReq:
		return req, true
	default:
		return seekRequest{}, false
	}
}

// retireLoop marks this generation's decode goroutine as gone and hands back a
// target that was queued meanwhile. The flag and the pop happen under p.mu —
// the same lock Seek takes to decide between "queue it" and "restart the
// source" — so a seek can never be queued to a loop that has already exited.
func (p *Player) retireLoop(gen uint64) (seekRequest, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gen != p.gen.curLoad() {
		return seekRequest{}, false
	}
	p.loopAlive = false
	select {
	case req := <-p.seekReq:
		return req, true
	default:
		return seekRequest{}, false
	}
}

// retireLoopQuiet is the deferred counterpart for exits other than EOF.
func (p *Player) retireLoopQuiet(gen uint64) {
	p.mu.Lock()
	if gen == p.gen.curLoad() {
		p.loopAlive = false
	}
	p.mu.Unlock()
}

// dropQueuedSeek forgets a target that belongs to a previous load/seek.
func (p *Player) dropQueuedSeek() {
	for {
		if _, ok := p.takeQueuedSeek(); !ok {
			return
		}
	}
}

// seekRebuildBudget bounds how many times a failed seek may rebuild the
// decoder before the track is declared broken. A failed seek leaves the
// demuxer in an undefined state, so retrying on the same context is not an
// option; one retry covers the transient CDN hiccup that caused it without
// letting a permanently unseekable source loop forever.
const seekRebuildBudget = 1

// resumeIntent reports whether a rebuilt pipeline should start producing
// sound. It follows the user's intent, not the machine's current permission to
// run the device: a rebuild after end of file must resume (the engine parked
// itself, the listener did not ask for that), while a rebuild after Pause must
// stay silent.
func (p *Player) resumeIntent() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.playIntent
}

// takeSeekRebuild consumes one rebuild attempt from the current load's budget.
func (p *Player) takeSeekRebuild() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seekRebuilds <= 0 {
		return false
	}
	p.seekRebuilds--
	return true
}

// rebuildAfterSeek replaces the decoder when an in-place seek failed.
//
// Seeking an HTTP source is a burst of Range reads (the demuxer's index search
// probes arbitrary offsets), so one slow reply surfaces inside FFmpeg as
// "Error reading HTTP response: Connection timed out" and then
// "read_timestamp() failed in the middle" — after which the demuxer's state is
// undefined. Failing the whole track there is what made a seek feel like it
// "killed" the song; a fresh open at the target is both safe and the same
// path a pending load takes.
func (p *Player) rebuildAfterSeek(gen uint64, at time.Duration, cause error) error {
	p.mu.Lock()
	stale := p.closed || gen != p.gen.curLoad()
	p.mu.Unlock()
	if stale {
		p.logf("seek rebuild skipped: generation moved on")
		return nil
	}
	resume := p.resumeIntent()
	if !p.takeSeekRebuild() {
		return fmt.Errorf("seek: %w", cause)
	}
	p.logf("seek to %s failed (%v); rebuilding the decoder at the target", at, cause)
	// Guard only the load generation: the attempt above read the newest queued
	// target under this same generation, and any seek that arrives late is
	// picked up by the rebuilt decode loop.
	return p.reopenIf(gen, 0, at, resume)
}

// rebuildAfterRead recovers a transient decode failure (a connection reset, a
// CDN dropping a range mid-song) by reopening the source at the position the
// listener actually heard. It reports whether recovery started; the caller
// then ends this generation. Permanently broken media exhausts the budget and
// fails normally.
func (p *Player) rebuildAfterRead(gen uint64, cause error) bool {
	p.mu.Lock()
	if p.closed || gen != p.gen.curLoad() || !p.meta.Seekable {
		p.mu.Unlock()
		return false
	}
	pos := p.clock.position(p.pulled.Load())
	p.mu.Unlock()
	resume := p.resumeIntent()
	if !p.takeSeekRebuild() {
		return false
	}
	p.logf("decoder read failed at %s (%v); reopening the source", pos, cause)
	if err := p.reopenIf(gen, 0, pos, resume); err != nil {
		p.failGen(gen, err)
	}
	return true
}

// SeekTo is the float-seconds convenience used by UI sliders.
func (p *Player) SeekTo(seconds float64) error {
	return p.Seek(time.Duration(seconds * float64(time.Second)))
}

// SetVolume sets the linear master gain. Changes ramp over 20 ms, so dragging a
// slider never zippers.
func (p *Player) SetVolume(v float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v < 0 {
		v = 0
	}
	p.volume = v
	if p.muted {
		return
	}
	p.outGain.SetLinear(v)
}

// Volume reports the linear master gain the user asked for.
func (p *Player) Volume() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.volume
}

// SetMuted silences the stream without touching the user's volume.
func (p *Player) SetMuted(muted bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.muted = muted
	if muted {
		p.outGain.SetLinear(0)
		return
	}
	p.outGain.SetLinear(p.volume)
}

// Muted reports the mute flag.
func (p *Player) Muted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.muted
}

// SetEQ installs the band list. Filters keep their state where the count and
// types match, so EQ changes do not restart decoding or click.
func (p *Player) SetEQ(bands []dsp.Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.chain.SetBands(bands)
}

// EQ reports the current bands.
func (p *Player) EQ() []dsp.Config { return p.chain.Bands() }

// SetEffects replaces the user effects chain that runs after the EQ.
func (p *Player) SetEffects(chain []dsp.Processor) { p.chain.SetChain(chain) }

// SetLimiter turns the safety limiter on or off.
func (p *Player) SetLimiter(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chain.SetLimiter(on, p.cfg.LimiterDB, p.cfg.LimiterAttack, p.cfg.LimiterRelease)
}

// SetBypass is a true bypass of the whole DSP chain.
func (p *Player) SetBypass(bypass bool) { p.chain.SetBypass(bypass) }

// DSPState snapshots the chain for the settings UI.
func (p *Player) DSPState() dsp.State { return p.chain.State() }

// Level drains the meter window: peaks, RMS and clip count since the last call.
func (p *Player) Level() dsp.Level { return p.chain.Level() }

// Device reports the endpoint the player is using.
func (p *Player) Device() output.Device {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.device
}

// Devices lists the endpoints visible to the selected driver.
func (p *Player) Devices() ([]output.Device, error) {
	p.mu.Lock()
	name := p.cfg.Driver
	p.mu.Unlock()
	return listDevices(name)
}

// SetDevice moves the session to another endpoint without dropping the track:
// the old stream is closed, a new one opens, and playback resumes at the same
// position.
func (p *Player) SetDevice(id string) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	pos := p.clock.position(p.pulled.Load())
	playing := p.state == StatePlaying || p.state == StateBuffering
	wantPlay := p.wantPlay
	p.mu.Unlock()

	if err := p.switchDriverDevice(id); err != nil {
		return err
	}
	p.pipe.Load().Interrupt()
	p.pipe.Load().Reset()
	p.chain.Reset()
	p.clock.rebase(pos, 0)
	p.pulled.Store(0)

	p.mu.Lock()
	p.base = pos
	if playing || wantPlay {
		p.wantPlay = true
		p.state = StateReady
	}
	p.mu.Unlock()
	p.nudge()
	if playing {
		p.publish(Event{Kind: KindDeviceChanged, Name: "device-changed"})
	}
	return nil
}

func listDevices(name string) ([]output.Device, error) {
	if name == "" {
		d, err := output.Default()
		if err != nil {
			return nil, err
		}
		return d.Devices()
	}
	d, ok := output.Lookup(name)
	if !ok {
		return nil, output.ErrUnavailable
	}
	return d.Devices()
}

// switchDriverDevice validates a new endpoint and, when an output is open,
// rebuilds it against the requested device.
func (p *Player) switchDriverDevice(id string) error {
	if id != "" {
		if _, _, err := output.FindDevice(id); err != nil {
			return err
		}
	}
	p.mu.Lock()
	out := p.out
	p.mu.Unlock()
	if out == nil {
		p.mu.Lock()
		p.cfg.DeviceID = id
		p.mu.Unlock()
		return nil
	}
	if err := p.rebuildOutput(id); err != nil {
		return err
	}
	p.mu.Lock()
	p.cfg.DeviceID = id
	p.mu.Unlock()
	return nil
}

// Snapshot is the state the UI renders.
func (p *Player) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshotLocked()
}

// Clock is a high-precision view of the playback timeline. BufferPos is the
// position of the last frame handed to the device; Position subtracts the
// output latency to estimate what the listener hears right now. Monotonic is
// Go's monotonic clock in nanoseconds, useful for aligning with Electron's
// performance.now() or MyGO's timing source.
type Clock struct {
	Position   time.Duration
	BufferPos  time.Duration
	Latency    time.Duration
	Monotonic  int64
	Frames     uint64
	SampleRate int
}

// Clock returns a point-in-time reading of the playback clock. It never blocks
// on the device: use it for lyric sync, spectrum alignment, or any consumer
// that needs sub-40ms precision beyond the event stream.
func (p *Player) Clock() Clock {
	p.mu.Lock()
	bufferPos := p.clock.position(p.pulled.Load())
	latency := time.Duration(0)
	if p.out != nil {
		latency = p.out.Latency()
	}
	frames := p.pulled.Load()
	sampleRate := pcm.DefaultSampleRate
	p.mu.Unlock()

	position := bufferPos - latency
	if position < 0 {
		position = 0
	}
	return Clock{
		Position:   position,
		BufferPos:  bufferPos,
		Latency:    latency,
		Monotonic:  time.Now().UnixNano(),
		Frames:     frames,
		SampleRate: sampleRate,
	}
}

// Events subscribes to the player. The returned cancel function unregisters the
// listener; the channel is closed by it.
func (p *Player) Events(size int) (<-chan Event, func()) {
	ch := p.hub.subscribe(size)
	return ch, func() { p.hub.unsubscribe(ch) }
}

// State reports the lifecycle.
func (p *Player) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// Close stops playback, releases the device session and the decoder, and
// permanently shuts the player down.
func (p *Player) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.wantPlay = false
	p.state = StateClosed
	p.loopAlive = false
	p.pending = nil
	out := p.out
	stream := p.stream
	p.out = nil
	p.stream = nil
	p.mu.Unlock()

	close(p.quit)
	p.pipe.Load().Close()
	p.decWG.Wait()
	p.monWG.Wait()

	var errs []error
	if stream != nil {
		if err := stream.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if out != nil {
		if err := out.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	p.publish(Event{Kind: KindClosed, Name: "closed"})
	return errors.Join(errs...)
}

// reopen reloads the current source at a position, used when a seek is not
// possible in place (live streams, reader-backed sources, dead devices).
func (p *Player) reopen(at time.Duration, resume bool) error {
	return p.reopenIf(0, 0, at, resume)
}

// reopenIf is reopen guarded by the generations the caller observed. When a
// newer load (or seek) already owns the pipeline it does nothing: the check and
// the generation bump happen under the same lock, so a recovery started for a
// stale seek/failure can never clobber a newer one. A zero generation means
// "don't check that counter".
func (p *Player) reopenIf(loadGen, seekGen uint64, at time.Duration, resume bool) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	if loadGen != 0 {
		if loadGen != p.gen.curLoad() || (seekGen != 0 && seekGen != p.gen.curSeek()) {
			p.mu.Unlock()
			return nil
		}
	}
	gen := p.gen.nextLoad()
	src := p.src
	p.eof = false
	p.primed = false
	p.wantPlay = resume
	p.base = at
	p.pulled.Store(0)
	p.bufferAt = 0
	p.clock = newClock(pcm.DefaultSampleRate)
	p.clock.base = at
	p.state = StateLoading
	p.loopAlive = true
	p.pipe.Load().Swap(gen)
	p.chain.Reset()
	stream := p.stream
	p.stream = nil
	p.mu.Unlock()
	if stream != nil {
		// Close in the background: the abandoned stream may be parked in a
		// network read (up to rw_timeout), and the new generation must not
		// wait for it before producing sound. Close takes the stream lock, so
		// it waits for that read to return and then releases the decoder.
		p.decWG.Add(1)
		go func() {
			defer p.decWG.Done()
			_ = stream.Close()
		}()
	}
	p.decWG.Add(1)
	go p.decodeLoop(context.Background(), gen, src, at)
	p.publishFrom(gen, Event{Kind: KindSeeked})
	return nil
}

// rebuildOutput replaces the device session, keeping the pipeline and the
// position alive. The old session pointer must be dropped first: ensureOutput
// would otherwise reuse a closed output.
func (p *Player) rebuildOutput(deviceID string) error {
	p.mu.Lock()
	out := p.out
	p.out = nil
	p.gen.nextEpoch()
	p.mu.Unlock()
	if out != nil {
		_ = out.Stop()
		_ = out.Close()
	}
	return p.ensureOutput(deviceID)
}

// fail records an asynchronous error and stops the machine.
func (p *Player) fail(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	gen := p.gen.curLoad()
	p.err = err
	p.state = StateFailed
	p.wantPlay = false
	snap := p.snapshotLocked()
	p.mu.Unlock()
	p.publishFrom(gen, Event{Kind: KindFailed, Name: "failed", Err: err.Error(), Snapshot: snap})
	if p.cfg.OnError != nil {
		p.cfg.OnError(err)
	}
}

func (p *Player) setState(st State, kind Kind) {
	p.mu.Lock()
	p.state = st
	snap := p.snapshotLocked()
	p.mu.Unlock()
	if kind != 0 {
		p.publish(Event{Kind: kind, Name: kind.String(), Snapshot: snap})
	}
}

func (p *Player) publish(ev Event) {
	p.mu.Lock()
	p.stampLocked(p.gen.curLoad(), &ev)
	p.mu.Unlock()
	p.hub.publish(ev)
}

// publishFrom publishes an event only while gen still owns the pipeline. Events
// produced by an asynchronous path (loaded/ended/closed) are stamped with the
// generation that produced them, not whatever is current when a blocked close
// finally returns: otherwise an old track's ended/closed arrives looking like
// it belongs to the track the user just switched to.
func (p *Player) publishFrom(gen uint64, ev Event) bool {
	p.mu.Lock()
	if p.closed || gen != p.gen.curLoad() {
		p.mu.Unlock()
		return false
	}
	p.stampLocked(gen, &ev)
	p.mu.Unlock()
	p.hub.publish(ev)
	return true
}

// stampLocked fills the wire fields common to every event. The caller must
// hold p.mu.
func (p *Player) stampLocked(gen uint64, ev *Event) {
	ev.Gen = gen
	ev.SeekGen = p.gen.curSeek()
	if ev.Snapshot.State == StateIdle {
		ev.Snapshot = p.snapshotLocked()
	}
}

// nudge asks the monitor to re-evaluate immediately.
func (p *Player) nudge() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Player) logf(format string, args ...any) {
	if p.cfg.Log != nil {
		p.cfg.Log(fmt.Sprintf(format, args...))
	}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// Diagnostics is the doctor output for the app: what can run here and why.
type Diagnostics struct {
	Version    string                `json:"version"`
	GOOS       string                `json:"goos"`
	GOARCH     string                `json:"goarch"`
	CgoEnabled bool                  `json:"cgo_enabled"`
	Decoders   []decode.Status       `json:"decoders"`
	Outputs    []output.DeviceStatus `json:"outputs"`
	Devices    []output.Device       `json:"devices"`
	DefaultOut string                `json:"default_output,omitempty"`
	DefaultDec string                `json:"default_decoder,omitempty"`
	Note       string                `json:"note,omitempty"`
}

// Diagnose probes the environment without opening a device.
func Diagnose() Diagnostics {
	d := Diagnostics{
		Version:    Version,
		GOOS:       goOS(),
		GOARCH:     goArch(),
		CgoEnabled: cgoEnabled(),
		Decoders:   decode.Statuses(),
		Outputs:    output.Statuses(),
	}
	if b, err := decode.Default(); err == nil {
		d.DefaultDec = b.Name()
	}
	if drv, err := output.Default(); err == nil {
		d.DefaultOut = drv.Name()
		if devs, err := drv.Devices(); err == nil {
			d.Devices = devs
		} else {
			d.Note = err.Error()
		}
	} else {
		d.Note = err.Error()
	}
	return d
}

// ListDevices enumerates every output endpoint of the default driver.
func ListDevices() ([]output.Device, error) { return listDevices("") }

// AllDevices enumerates every driver's endpoints.
func AllDevices() ([]output.Device, error) { return output.AllDevices() }

// DecodeBackends lists the decoder registrations.
func DecodeBackends() []decode.Status { return decode.Statuses() }

// OutputDrivers lists the output registrations.
func OutputDrivers() []output.DeviceStatus { return output.Statuses() }
