package liino

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/team-quaver/liino/decode"
	"github.com/team-quaver/liino/output"
	"github.com/team-quaver/liino/pcm"
)

// Read implements output.Source: the device callback drains the pipe and never
// takes the player lock, so a slow API call cannot starve the audio thread.
//
// The pipe carries the decoded source layout (5.1, 7.1, 7.1.4, ...). When the
// endpoint was opened for a different channel count — most commonly a stereo
// sink — this is where the fold happens, not in the decoder: swresample cannot
// rematrix layouts it does not know, and dropping channels there made 12-channel
// Opus ("臻品全景声 7.1") produce literally zero frames.
func (p *Player) Read(dst []float32) int {
	pipe := p.pipe.Load()
	inCh := pipe.Format().Channels
	outCh := int(p.outChannels.Load())
	if outCh <= 0 {
		outCh = pcm.DefaultChannels
	}
	if inCh == outCh {
		n := pipe.Read(dst)
		if n > 0 {
			p.outGain.Process(dst[:n*outCh], outCh)
			p.pulled.Add(uint64(n))
		}
		return n
	}
	frames := len(dst) / outCh
	if frames <= 0 {
		return 0
	}
	need := frames * inCh
	if cap(p.readBuf) < need {
		p.readBuf = make([]float32, need)
	}
	buf := p.readBuf[:need]
	n := pipe.Read(buf)
	if n <= 0 {
		return 0
	}
	pcm.MapChannels(dst[:n*outCh], buf[:n*inCh], n, inCh, outCh)
	p.outGain.Process(dst[:n*outCh], outCh)
	p.pulled.Add(uint64(n))
	return n
}

// Reset implements output.Source.
func (p *Player) Reset() { p.pulled.Store(0) }

// ensurePipe makes the pipe carry the given channel count. The callback may
// still be draining the old pipe; the pointer swap is atomic and the old pipe
// is interrupted so a producer blocked on it wakes up.
func (p *Player) ensurePipe(gen uint64, channels int) {
	if channels <= 0 {
		channels = pcm.DefaultChannels
	}
	cur := p.pipe.Load()
	if cur != nil && cur.Format().Channels == channels {
		return
	}
	next := pcm.NewPipe(p.cfg.RingFrames, pcm.Format{SampleRate: pcm.DefaultSampleRate, Channels: channels})
	next.Swap(gen)
	p.pipe.Store(next)
	if cur != nil {
		cur.Interrupt()
	}
}

// decodeLoop owns one decoder session: it opens the source, runs the DSP chain
// over the decoded PCM and feeds the pipe until the generation is superseded.
func (p *Player) decodeLoop(ctx context.Context, gen uint64, src decode.Source, startAt time.Duration) {
	defer p.decWG.Done()

	if p.stale(gen) {
		return
	}
	backend, err := p.resolveDecoder()
	if err != nil {
		p.failGen(gen, fmt.Errorf("decoder: %w", err))
		return
	}
	stream, err := backend.Open(ctx, src, p.decodeOptions())
	if err != nil {
		if p.stale(gen) {
			return
		}
		p.failGen(gen, fmt.Errorf("open %s: %w", src.Label(), err))
		return
	}
	if p.stale(gen) {
		_ = stream.Close()
		return
	}

	p.mu.Lock()
	p.stream = stream
	p.decoderName = backend.Name()
	p.meta = stream.Meta()
	// The pipe must match what the decoder actually emits. That is the stream's
	// Format() (canonical 48 kHz + source channels for native, stereo for the
	// wasm helper), not the container's Meta().
	srcCh := stream.Format().Channels
	if srcCh <= 0 {
		srcCh = p.meta.Channels
	}
	if srcCh <= 0 {
		srcCh = pcm.DefaultChannels
	}
	p.chain.SetFormat(pcm.DefaultSampleRate, srcCh)
	// The decoder keeps the source layout. Swap the pipe if this track's
	// channel count differs from the previous one; Read folds it for the
	// endpoint when necessary (see Read).
	p.ensurePipe(gen, srcCh)
	seekable := p.meta.Seekable
	p.mu.Unlock()

	// Seek **outside the player lock**: on an HTTP source this reads ranges from
	// the network and can stall for seconds. Holding mu here blocks every FFI
	// call the host makes on its UI thread (clock/snapshot/nextEvent) — the app
	// window stays painted while the main process is frozen and silent.
	if startAt > 0 && seekable {
		if _, err := stream.Seek(startAt); err != nil {
			_ = stream.Close()
			p.failGen(gen, fmt.Errorf("seek to start: %w", err))
			return
		}
	}

	p.mu.Lock()
	if p.state == StateLoading {
		p.state = StateReady
	}
	snap := p.snapshotLocked()
	p.mu.Unlock()
	p.publish(Event{Kind: KindLoaded, Name: "loaded", Snapshot: snap})
	p.logf("loaded %s via %s (%s, %d Hz %d ch, %s)", src.Label(), backend.Name(),
		p.meta.Codec, p.meta.SampleRate, p.meta.Channels, p.meta.Duration)

	buf := make([]float32, 0, pcm.ChunkFrames*srcCh)
	for {
		// Seeks are executed here: the decoder context is single-goroutine.
		if err := p.applyPendingSeek(gen, stream); err != nil {
			if !errors.Is(err, decode.ErrSeekUnsupported) {
				p.failGen(gen, err)
			}
			return
		}

		chunk, err := stream.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				p.markEOF(gen)
				return
			}
			if p.stale(gen) {
				return
			}
			p.failGen(gen, err)
			return
		}
		if p.stale(gen) {
			return
		}
		if chunk.Frames <= 0 {
			continue
		}
		ch := chunk.Format.Channels
		if ch <= 0 {
			ch = pcm.DefaultChannels
		}
		// Raw/live sources sometimes only reveal their layout on the first
		// decoded frame. Keep the pipe and DSP in sync when that happens.
		if p.pipe.Load().Format().Channels != ch {
			p.ensurePipe(gen, ch)
			p.chain.SetFormat(pcm.DefaultSampleRate, ch)
		}
		n := chunk.Frames * ch
		buf = append(buf[:0], chunk.Data[:n]...)
		p.chain.Process(buf, ch)
		if _, err := p.pipe.Load().WriteFrames(gen, buf, chunk.Frames); err != nil {
			switch {
			case p.stale(gen), errors.Is(err, pcm.ErrPipeStale), errors.Is(err, pcm.ErrPipeClosed):
				return
			case errors.Is(err, pcm.ErrPipeInterrupted):
				// A seek or device switch dropped the buffer mid-write: throw
				// this chunk away and pick up the new position next round.
				p.logf("producer interrupted, dropping %d frames", chunk.Frames)
				continue
			default:
				p.failGen(gen, err)
				return
			}
		}
		p.decoded.Add(uint64(chunk.Frames))
		p.nudge()
	}

	// The stream is closed by the next generation's owner: hardStop and reopen
	// hand it over rather than racing this goroutine on the decoder context.
}

// applyPendingSeek performs a queued seek on the decoder owned by this
// generation. It returns a non-nil error when playback must stop.
func (p *Player) applyPendingSeek(gen uint64, stream decode.Stream) error {
	var target time.Duration
	select {
	case target = <-p.seekReq:
	default:
		return nil
	}
	if p.stale(gen) {
		return nil
	}
	p.mu.Lock()
	seekable := p.meta.Seekable
	pos := p.clock.base
	p.mu.Unlock()
	if pos != target {
		p.mu.Lock()
		p.clock.base = target
		p.mu.Unlock()
		p.pulled.Store(0)
	}
	if !seekable {
		p.logf("source is not seekable, reloading at %s", target)
		return fmt.Errorf("%w: reload", decode.ErrSeekUnsupported)
	}
	if _, err := stream.Seek(target); err != nil {
		if errors.Is(err, decode.ErrSeekUnsupported) {
			return err
		}
		return fmt.Errorf("seek: %w", err)
	}
	p.pipe.Load().Reset()
	p.nudge()
	p.logf("seeked to %s", target)
	return nil
}

// resolveDecoder picks the configured backend, or the first usable one.
func (p *Player) resolveDecoder() (decode.Backend, error) {
	p.mu.Lock()
	name := p.cfg.Decoder
	p.mu.Unlock()
	if name != "" {
		if b, ok := decode.Lookup(name); ok {
			if ok, note := b.Probe(); ok {
				return b, nil
			} else if p.cfg.Decoder != "" {
				return nil, fmt.Errorf("%w: %s: %s", decode.ErrUnavailable, name, note)
			}
		}
	}
	return decode.Default()
}

func (p *Player) decodeOptions() decode.Options {
	return decode.Options{
		ChunkFrames:     pcm.ChunkFrames,
		OpenTimeout:     15 * time.Second,
		ProbeBytes:      1 << 20,
		AnalyzeDuration: 2 * time.Second,
		Log:             p.cfg.Log,
	}
}

func (p *Player) stale(gen uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed || gen != p.gen.curLoad()
}

// markEOF records end of stream so the monitor can finish on drain.
func (p *Player) markEOF(gen uint64) {
	if p.stale(gen) {
		return
	}
	p.mu.Lock()
	p.eof = true
	p.mu.Unlock()
	p.pipe.Load().Close()
	p.nudge()
	p.logf("decoder reached end of stream")
}

func (p *Player) failGen(gen uint64, err error) {
	if p.stale(gen) || err == nil {
		return
	}
	p.fail(err)
}

// ensureOutput opens the device session lazily: New() must not grab an audio
// endpoint, otherwise a headless process could never even construct a player.
func (p *Player) ensureOutput(deviceID string) error {
	// Serialise opens, but never hold p.mu across output.Open: PipeWire stream
	// creation waits on the session manager and can block arbitrarily. With mu
	// held there, every FFI call from the host (clock/snapshot/nextEvent) parks
	// on the Electron main thread — the window keeps painting, audio never
	// starts, and switching backend appears dead.
	p.openMu.Lock()
	defer p.openMu.Unlock()

	p.mu.Lock()
	if p.out != nil {
		p.mu.Unlock()
		return nil
	}
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	driverName := p.cfg.Driver
	periodFrames := p.cfg.PeriodFrames
	appName := p.cfg.AppName
	streamName := p.cfg.StreamName
	logf := p.cfg.Log
	// Negotiate against what the decoder actually feeds the pipe, not the
	// container metadata (wasm downmixes, native preserves).
	sourceCh := p.pipe.Load().Format().Channels
	if sourceCh <= 0 {
		sourceCh = p.meta.Channels
	}
	p.mu.Unlock()

	// Device enumeration can be slow (WASAPI/Core Audio); do it after releasing
	// p.mu so FFI calls from the main thread never wait on it.
	if driverName == "" {
		if d, err := output.Default(); err == nil {
			driverName = d.Name()
		}
	}
	outCh := outputChannelPlan(driverName, deviceID, sourceCh)
	opts := output.Options{
		DeviceID:     deviceID,
		Format:       pcm.Format{SampleRate: pcm.DefaultSampleRate, Channels: outCh},
		PeriodFrames: periodFrames,
		AppName:      appName,
		StreamName:   streamName,
		Source:       p,
		OnUnderrun:   func(int) { p.onUnderrun() },
		OnError: func(err error) {
			if err != nil {
				p.fail(err)
			}
		},
		Log: logf,
	}

	out, driver, err := output.Open(driverName, opts)
	if err != nil && outCh > pcm.DefaultChannels {
		// Shared-mode endpoints on Windows/macOS are fixed to their mix format;
		// asking for 12 channels there can fail outright. Keep the track (the
		// decoder still has all channels) and fold it to stereo instead.
		p.logf("output open at %d ch failed (%v); retrying stereo", outCh, err)
		outCh = pcm.DefaultChannels
		opts.Format.Channels = outCh
		out, driver, err = output.Open(driverName, opts)
	}
	if err != nil {
		return err
	}

	dev := out.Device()
	if dev.ID == "" {
		// Remember the concrete endpoint even when the default was requested:
		// the device watcher needs an identity to check for disappearance.
		if devs, err := driver.Devices(); err == nil {
			for _, d := range devs {
				if d.Default || dev.Name == "" {
					dev = d
					break
				}
			}
		}
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = out.Close()
		return ErrClosed
	}
	if p.out != nil {
		// Another caller won the race while we were opening; drop ours.
		p.mu.Unlock()
		_ = out.Close()
		return nil
	}
	p.out = out
	p.driver = driver
	p.cfg.Driver = driver.Name()
	p.device = dev
	p.outChannels.Store(int32(outCh))
	p.mu.Unlock()

	p.logf("output %s opened on %q (latency %s)", driver.Name(), dev.Name, out.Latency())
	return nil
}

// outputChannelPlan picks the channel count for a new endpoint. Multichannel
// sources are kept intact when the backend can carry them; otherwise the
// player folds them to stereo in Read (never in the decoder — swresample
// cannot rematrix unknown layouts such as 7.1.4 Opus).
func outputChannelPlan(driverName, deviceID string, sourceCh int) int {
	if sourceCh <= pcm.DefaultChannels {
		return pcm.DefaultChannels
	}
	switch driverName {
	case "pipewire":
		// The stream node can advertise the source layout and PipeWire inserts
		// a channelmixer when the sink is stereo, so keep all channels.
		return sourceCh
	case "null":
		return pcm.DefaultChannels
	}
	d, ok := output.Lookup(driverName)
	if !ok {
		return pcm.DefaultChannels
	}
	devs, err := d.Devices()
	if err != nil {
		return pcm.DefaultChannels
	}
	target := output.Device{}
	for _, dev := range devs {
		if deviceID != "" && dev.ID == deviceID {
			target = dev
			break
		}
		if target.ID == "" && dev.Default {
			target = dev
		}
	}
	if target.ID == "" && len(devs) > 0 {
		target = devs[0]
	}
	maxCh := 0
	for _, f := range target.Formats {
		if f.Channels > maxCh {
			maxCh = f.Channels
		}
	}
	if maxCh >= sourceCh {
		return sourceCh
	}
	return pcm.DefaultChannels
}

// startDevice primes the output and reports the transition.
func (p *Player) startDevice() {
	p.mu.Lock()
	out := p.out
	deviceID := p.cfg.DeviceID
	p.mu.Unlock()
	if out == nil {
		if err := p.ensureOutput(deviceID); err != nil {
			p.fail(err)
			return
		}
		p.mu.Lock()
		out = p.out
		p.mu.Unlock()
	}
	if err := out.Start(); err != nil {
		p.fail(fmt.Errorf("output start: %w", err))
		return
	}
	p.publish(Event{Kind: KindPlaying, Name: "playing"})
}

// onUnderrun converts a device-side gap into a buffering transition.
func (p *Player) onUnderrun() {
	p.mu.Lock()
	if p.closed || p.state != StatePlaying {
		p.mu.Unlock()
		return
	}
	p.state = StateBuffering
	p.mu.Unlock()
	p.publish(Event{Kind: KindBuffering, Name: "buffering"})
}

// monitor is the single state machine goroutine: preroll, buffering, drain,
// deferred fades and device health.
func (p *Player) monitor() {
	defer p.monWG.Done()
	tick := time.NewTicker(p.cfg.MonitorInterval)
	defer tick.Stop()
	devTick := time.NewTicker(p.cfg.DevicePolling)
	defer devTick.Stop()

	for {
		select {
		case <-p.quit:
			return
		case <-p.wake:
			p.evaluate()
		case <-tick.C:
			p.evaluate()
		case <-devTick.C:
			p.checkDevice()
		}
	}
}

func (p *Player) evaluate() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	// A queued fade-out finished: run the deferred action now.
	if p.pending != nil && p.chain.FadeDone() {
		done := p.pending
		p.pending = nil
		p.mu.Unlock()
		done()
		return
	}

	avail := p.pipe.Load().Available()
	switch p.state {
	case StateLoading, StateReady:
		if p.wantPlay && (avail >= p.cfg.PrerollFrames || p.eof) {
			p.state = StatePlaying
			p.mu.Unlock()
			p.startDevice()
			return
		}
	case StateBuffering:
		if avail >= p.cfg.PrerollFrames/2 {
			p.state = StatePlaying
			p.mu.Unlock()
			p.publish(Event{Kind: KindReady, Name: "ready"})
			return
		}
		if p.eof && avail == 0 {
			p.finishLocked()
			return
		}
	case StatePlaying:
		if p.eof && avail == 0 {
			p.finishLocked()
			return
		}
		if avail == 0 && !p.eof {
			p.state = StateBuffering
			p.mu.Unlock()
			p.publish(Event{Kind: KindBuffering, Name: "buffering"})
			return
		}
	default:
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
}

// finishLocked transitions to Ended; the caller must hold p.mu.
func (p *Player) finishLocked() {
	out := p.out
	p.state = StateEnded
	p.wantPlay = false
	snap := p.snapshotLocked()
	p.mu.Unlock()
	if out != nil {
		_ = out.Stop()
	}
	p.publish(Event{Kind: KindEnded, Name: "ended", Snapshot: snap})
	p.logf("playback ended")
}

// checkDevice watches for a disappeared endpoint, or the system default moving
// to another device while we follow it, and reopens without losing position.
func (p *Player) checkDevice() {
	p.mu.Lock()
	if p.closed || p.out == nil {
		p.mu.Unlock()
		return
	}
	state := p.state
	driver := p.driver
	current := p.device
	following := p.cfg.DeviceID == ""
	p.mu.Unlock()
	if state != StatePlaying && state != StateBuffering && state != StatePaused {
		return
	}
	if driver == nil {
		return
	}
	devs, err := driver.Devices()
	if err != nil {
		p.logf("device poll failed: %v", err)
		return
	}
	present := false
	var fallback output.Device
	for _, d := range devs {
		if d.ID == current.ID {
			present = true
		}
		if d.Default {
			fallback = d
		}
	}
	if present && (!following || fallback.ID == "" || fallback.ID == current.ID) {
		return
	}
	target := current
	if !present || following {
		target = fallback
	}
	p.logf("output %q no longer usable, switching to %q", current.Name, target.Name)
	p.publish(Event{Kind: KindDeviceLost, Name: "device-lost"})
	if err := p.rebuildOutput(target.ID); err != nil {
		p.fail(fmt.Errorf("recover device: %w", err))
		return
	}
	p.mu.Lock()
	p.cfg.DeviceID = ""
	if following {
		// Keep following the system default rather than pinning the new one.
		p.cfg.DeviceID = ""
	} else {
		p.cfg.DeviceID = target.ID
	}
	pos := p.clock.position(p.pulled.Load())
	p.pulled.Store(0)
	p.clock = newClock(pcm.DefaultSampleRate)
	p.clock.base = pos
	p.wantPlay = state == StatePlaying || state == StateBuffering
	p.state = StateReady
	p.mu.Unlock()
	p.pipe.Load().Reset()
	p.pipe.Load().Interrupt()
	if p.wantPlay {
		p.nudge()
	}
	p.publish(Event{Kind: KindDeviceChanged, Name: "device-changed"})
}

// snapshotLocked builds the public state view; the caller must hold p.mu.
func (p *Player) snapshotLocked() Snapshot {
	snap := Snapshot{
		State:         p.state,
		Generation:    p.gen.curLoad(),
		Position:      p.clock.position(p.pulled.Load()),
		Duration:      p.meta.Duration,
		Buffered:      p.bufferedLocked(),
		Volume:        p.volume,
		Muted:         p.muted,
		Decoder:       p.decoderName,
		Codec:         p.meta.Codec,
		SampleRate:    pcm.DefaultSampleRate,
		Channels:      snapshotChannels(p.meta.Channels),
		Live:          p.meta.Live,
		FramesWritten: p.pulled.Load(),
		Drops:         p.pipe.Load().Dropped(),
	}
	if p.out != nil {
		snap.Driver = p.out.Device().Driver
		if snap.Driver == "" && p.driver != nil {
			snap.Driver = p.driver.Name()
		}
		snap.Device = p.device.Name
		snap.Underruns = p.out.Underruns()
		snap.Latency = p.out.Latency()
	}
	if p.err != nil {
		snap.Error = p.err.Error()
	}
	level := p.chain.LevelPeek()
	snap.PeakL, snap.PeakR = dbOf(level.PeakL), dbOf(level.PeakR)
	snap.Clips = level.Clips
	return snap
}

// snapshotChannels keeps the reported layout honest: the decoder preserves the
// source count, so a 12-channel Opus reports 12 even when Read folds it to a
// 2-channel endpoint.
func snapshotChannels(n int) int {
	if n <= 0 {
		return pcm.DefaultChannels
	}
	return n
}

func dbOf(v float64) float64 {
	if v <= 0 {
		return -120
	}
	db := 20 * math.Log10(v)
	if db < -120 {
		return -120
	}
	return db
}

// bufferedLocked converts the pipe occupancy into a duration.
func (p *Player) bufferedLocked() time.Duration {
	frames := float64(p.pipe.Load().Available())
	return time.Duration(frames / float64(pcm.DefaultSampleRate) * float64(time.Second))
}
