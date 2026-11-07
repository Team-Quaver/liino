package liino

import (
	"context"
	"errors"
	"io"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/team-quaver/liino/decode"
	"github.com/team-quaver/liino/dsp"
	"github.com/team-quaver/liino/output"
	"github.com/team-quaver/liino/pcm"
)

// fakeBackend decodes a synthetic tone so the engine can be tested without
// FFmpeg or a sound card.
type fakeBackend struct {
	mu      sync.Mutex
	opened  int
	streams []*fakeStream
	failErr error
	seekErr error
	// seekErrOnOpen makes only the stream from this 1-based Open reject seeks,
	// modelling a transient network failure: the rebuilt decoder seeks fine.
	seekErrOnOpen int
	// readErrOnOpen + readErrAfter make that Open's stream fail a read once it
	// has produced readErrAfter frames (a CDN reset mid-song).
	readErrOnOpen int
	readErrAfter  int
	// unseekable models a stream whose container has no index (live-ish HTTP):
	// seeking is refused, so a resume offset cannot be honoured.
	unseekable bool
}

type fakeStream struct {
	// Real streams serialise Read/Seek/Close on their own mutex (the engine
	// seeks on a goroutine while the producer reads); mirror that here so the
	// race detector only sees production races.
	mu        sync.Mutex
	frames    int // total frames in the synthetic track
	produced  int
	seeked    []time.Duration
	seekErr   error
	readErr   error
	readAfter int
	closed    bool
	chunkSize int
	meta      decode.Meta
}

func (b *fakeBackend) Name() string  { return "fake" }
func (b *fakeBackend) Priority() int { return -1 }
func (b *fakeBackend) Probe() (bool, string) {
	return true, "synthetic tone"
}

func (b *fakeBackend) Open(context.Context, decode.Source, decode.Options) (decode.Stream, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opened++
	if b.failErr != nil {
		return nil, b.failErr
	}
	total := pcm.DefaultSampleRate * 2 // two seconds
	s := &fakeStream{
		frames:    total,
		chunkSize: pcm.ChunkFrames,
		meta: decode.Meta{
			Codec:      "fake",
			SampleRate: pcm.DefaultSampleRate,
			Channels:   pcm.DefaultChannels,
			Duration:   2 * time.Second,
			Seekable:   !b.unseekable,
		},
	}
	switch {
	case b.seekErr != nil:
		s.seekErr = b.seekErr
	case b.seekErrOnOpen != 0 && b.seekErrOnOpen == b.opened:
		s.seekErr = errors.New("range read timed out")
	}
	if b.readErrOnOpen != 0 && b.readErrOnOpen == b.opened {
		s.readErr = errors.New("connection reset by peer")
		s.readAfter = b.readErrAfter
	}
	b.streams = append(b.streams, s)
	return s, nil
}

// streamAt returns the nth opened stream, or nil.
func (b *fakeBackend) streamAt(n int) *fakeStream {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 0 || n >= len(b.streams) {
		return nil
	}
	return b.streams[n]
}

func (s *fakeStream) Format() pcm.Format { return pcm.DefaultFormat() }
func (s *fakeStream) Meta() decode.Meta  { return s.meta }

func (s *fakeStream) Read() (pcm.Chunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return pcm.Chunk{}, decode.ErrNotOpen
	}
	if s.readErr != nil && s.produced >= s.readAfter {
		return pcm.Chunk{}, s.readErr
	}
	if s.produced >= s.frames {
		return pcm.Chunk{}, io.EOF
	}
	n := min(s.chunkSize, s.frames-s.produced)
	out := pcm.Chunk{Data: make([]float32, n*pcm.DefaultChannels), Frames: n, Format: pcm.DefaultFormat()}
	for i := 0; i < n; i++ {
		t := float64(s.produced+i) / pcm.DefaultSampleRate
		v := float32(0.2 * math.Sin(2*math.Pi*220*t))
		out.Data[i*2] = v
		out.Data[i*2+1] = v
	}
	s.produced += n
	return out, nil
}

func (s *fakeStream) Seek(t time.Duration) (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seekErr != nil {
		return 0, s.seekErr
	}
	s.seeked = append(s.seeked, t)
	s.produced = int(t.Seconds() * pcm.DefaultSampleRate)
	return t, nil
}

func (s *fakeStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *fakeStream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func registerFake(t *testing.T, b *fakeBackend) {
	t.Helper()
	decode.Register(b)
	t.Cleanup(func() { decode.Unregister("fake") })
}

func testConfig(decoder string) Config {
	return Config{
		Decoder:         decoder,
		Driver:          "null",
		RingFrames:      pcm.DefaultSampleRate,
		PrerollFrames:   480,
		PeriodFrames:    240,
		MonitorInterval: 5 * time.Millisecond,
		DevicePolling:   time.Hour,
		FadeOut:         0,
	}
}

func waitState(t *testing.T, p *Player, ch <-chan Event, want State, timeout time.Duration) Snapshot {
	t.Helper()
	deadline := time.After(timeout)
	for {
		snap := p.Snapshot()
		if snap.State == want {
			return snap
		}
		select {
		case ev, ok := <-ch:
			if !ok {
				return snap
			}
			if ev.Kind == KindFailed {
				t.Fatalf("player failed: %s", ev.Err)
			}
		case <-deadline:
			t.Fatalf("timeout waiting for state %s, last=%+v", want, snap)
		}
	}
}

// TestBufferingRecoversWhenFramesFlow covers the real-time decoder path: after
// a seek the pipe never refills to half the preroll depth, but the device keeps
// consuming frames. Recovery must follow pulled frames, not pipe occupancy, or
// the UI stays on the buffering spinner while audio is already playing.
func TestBufferingRecoversWhenFramesFlow(t *testing.T) {
	registerFake(t, &fakeBackend{})
	cfg := testConfig("fake")
	cfg.MonitorInterval = time.Hour
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.mu.Lock()
	p.cfg.PrerollFrames = 9600
	p.state = StateBuffering
	p.bufferAt = 0
	p.mu.Unlock()
	p.pulled.Store(uint64(p.cfg.PeriodFrames))
	p.evaluate()

	if snap := p.Snapshot(); snap.State != StatePlaying {
		t.Fatalf("state = %s, want playing after %d frames reached the device", snap.State, p.cfg.PeriodFrames)
	}
}

// A momentary empty pipe between two callbacks is not a stall. The device's
// OnUnderrun callback is the authoritative signal; polling Available() at the
// monitor tick used to flip a healthy real-time stream into buffering.
func TestEmptyPipeDoesNotPausePlaying(t *testing.T) {
	registerFake(t, &fakeBackend{})
	cfg := testConfig("fake")
	cfg.MonitorInterval = time.Hour
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.mu.Lock()
	p.state = StatePlaying
	p.mu.Unlock()
	p.pipe.Load().Reset()
	p.evaluate()

	if snap := p.Snapshot(); snap.State != StatePlaying {
		t.Fatalf("state = %s, want playing while the output callback is still running", snap.State)
	}
}

// Recovery must still wait when nothing reaches the device: a real network
// stall has pulled=0 past bufferAt, so the spinner should stay honest.
func TestBufferingWaitsWithoutFrames(t *testing.T) {
	registerFake(t, &fakeBackend{})
	cfg := testConfig("fake")
	cfg.MonitorInterval = time.Hour
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.mu.Lock()
	p.cfg.PrerollFrames = 9600
	p.state = StateBuffering
	p.bufferAt = 0
	p.mu.Unlock()
	p.pulled.Store(0)
	p.evaluate()

	if snap := p.Snapshot(); snap.State != StateBuffering {
		t.Fatalf("state = %s, want buffering while no frames reach the device", snap.State)
	}
}

func TestPlayerPlaysToCompletion(t *testing.T) {
	fake := &fakeBackend{}
	registerFake(t, fake)

	p, err := New(testConfig("fake"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	events, cancel := p.Events(128)
	defer cancel()

	if err := p.Open(context.Background(), decode.Source{URI: "fake://tone"}, OpenOptions{AutoPlay: true}); err != nil {
		t.Fatal(err)
	}
	waitState(t, p, events, StatePlaying, 2*time.Second)

	// Two seconds of audio at the null device's nominal rate.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if p.Snapshot().State == StateEnded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap := p.Snapshot()
	if snap.State != StateEnded {
		t.Fatalf("state = %s, want ended", snap.State)
	}
	if snap.FramesWritten < pcm.DefaultSampleRate*3/2 {
		t.Fatalf("frames written = %d, want most of 2s of audio", snap.FramesWritten)
	}
	if snap.Position < 1500*time.Millisecond {
		t.Fatalf("position = %s, want > 1.5s", snap.Position)
	}
	if snap.Duration != 2*time.Second {
		t.Fatalf("duration = %s", snap.Duration)
	}
}

// A seek that arrives after the decoder reached EOF must restart playback.
// The decode goroutine has retired at that point; queueing the target would
// leave nobody to read it (the "seek near the end freezes the track" bug).
func TestSeekAfterEndRestartsPlayback(t *testing.T) {
	fake := &fakeBackend{}
	registerFake(t, fake)

	p, err := New(testConfig("fake"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	events, cancel := p.Events(128)
	defer cancel()

	if err := p.Open(context.Background(), decode.Source{URI: "fake://tone"}, OpenOptions{AutoPlay: true}); err != nil {
		t.Fatal(err)
	}
	waitState(t, p, events, StateEnded, 6*time.Second)

	if err := p.Seek(500 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := p.Play(); err != nil {
		t.Fatal(err)
	}
	waitState(t, p, events, StatePlaying, 3*time.Second)
	if snap := p.Snapshot(); snap.Position < 450*time.Millisecond {
		t.Fatalf("position after restart = %s, want ~0.5s", snap.Position)
	}
}

func TestPlayerSeekMovesPosition(t *testing.T) {
	fake := &fakeBackend{}
	registerFake(t, fake)

	p, err := New(testConfig("fake"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	events, cancel := p.Events(128)
	defer cancel()

	if err := p.Open(context.Background(), decode.Source{URI: "fake://tone"}, OpenOptions{AutoPlay: true}); err != nil {
		t.Fatal(err)
	}
	waitState(t, p, events, StatePlaying, 2*time.Second)
	if err := p.Seek(1500 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.Snapshot().Position < 1500*time.Millisecond {
		time.Sleep(5 * time.Millisecond)
	}
	if snap := p.Snapshot(); snap.Position < 1450*time.Millisecond {
		t.Fatalf("position after seek = %s", snap.Position)
	}
}

// A failed in-place seek must not kill the track. Seeking an HTTP source is a
// burst of Range reads; when one times out FFmpeg fails the seek and leaves
// the demuxer in an undefined state. The player must rebuild the decoder at
// the target (and keep playing) instead of reporting a fatal error — this is
// the "seek ruins the whole song" regression.
func TestSeekFailureRebuildsDecoderAtTarget(t *testing.T) {
	fake := &fakeBackend{seekErrOnOpen: 1}
	registerFake(t, fake)

	p, err := New(testConfig("fake"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	events, cancel := p.Events(128)
	defer cancel()

	if err := p.Open(context.Background(), decode.Source{URI: "fake://tone"}, OpenOptions{AutoPlay: true}); err != nil {
		t.Fatal(err)
	}
	waitState(t, p, events, StatePlaying, 2*time.Second)

	if err := p.Seek(1500 * time.Millisecond); err != nil {
		t.Fatal(err)
	}

	// The seek is asynchronous and the clock is already at 1.5 s, so wait for
	// the rebuild itself rather than for the position.
	deadline := time.Now().Add(3 * time.Second)
	opened := 0
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		opened = fake.opened
		fake.mu.Unlock()
		if opened >= 2 {
			break
		}
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("event stream closed")
			}
			if ev.Kind == KindFailed {
				t.Fatalf("failed seek killed the track: %s", ev.Err)
			}
		case <-time.After(10 * time.Millisecond):
		}
	}
	if opened < 2 {
		t.Fatalf("decoder opened %d times, want a rebuild", opened)
	}

	waitState(t, p, events, StatePlaying, 2*time.Second)
	if snap := p.Snapshot(); snap.State == StateFailed {
		t.Fatalf("failed seek killed the track: %s", snap.Error)
	} else if snap.Position < 1450*time.Millisecond {
		t.Fatalf("position after rebuilt seek = %s, want ~1.5s", snap.Position)
	}
}

// The same recovery applies to a load that starts at an offset (resume /
// window rebuild): the initial seek runs on a cold demuxer and can time out
// just like a user seek.
func TestInitialSeekFailureReopensSource(t *testing.T) {
	fake := &fakeBackend{seekErrOnOpen: 1}
	registerFake(t, fake)

	p, err := New(testConfig("fake"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	events, cancel := p.Events(128)
	defer cancel()

	if err := p.Open(context.Background(), decode.Source{URI: "fake://tone"}, OpenOptions{
		AutoPlay: true,
		StartAt:  1500 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	waitState(t, p, events, StatePlaying, 3*time.Second)

	fake.mu.Lock()
	opened := fake.opened
	fake.mu.Unlock()
	if opened < 2 {
		t.Fatalf("decoder opened %d times, want a retry", opened)
	}
	if snap := p.Snapshot(); snap.Position < 1450*time.Millisecond {
		t.Fatalf("position after rebuilt initial seek = %s, want ~1.5s", snap.Position)
	}
}

// A source that cannot seek must not claim the requested offset. Playback
// starts at 0, so the clock has to follow it back to 0; otherwise the UI shows
// the breakpoint while the audio restarts from the top (the Windows/WMF HTTP
// resume bug).
func TestUnseekableSourceStartsClockAtZero(t *testing.T) {
	fake := &fakeBackend{unseekable: true}
	registerFake(t, fake)

	p, err := New(testConfig("fake"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	events, cancel := p.Events(128)
	defer cancel()

	if err := p.Open(context.Background(), decode.Source{URI: "fake://tone"}, OpenOptions{
		AutoPlay: true,
		StartAt:  1500 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	waitState(t, p, events, StatePlaying, 3*time.Second)

	if snap := p.Snapshot(); snap.Position >= time.Second {
		t.Fatalf("position = %s, want playback from 0 (source is not seekable)", snap.Position)
	}
}

// When the seek retry budget is spent, a stream that still cannot seek must
// fall back to playing from the top — not be declared dead. Otherwise a source
// that advertises Seekable but rejects SetCurrentPosition (some HTTP servers)
// turns every resume into "failed to play".
func TestInitialSeekFailureFallsBackToStart(t *testing.T) {
	fake := &fakeBackend{seekErr: errors.New("seek: range not satisfiable")}
	registerFake(t, fake)

	p, err := New(testConfig("fake"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	events, cancel := p.Events(128)
	defer cancel()

	if err := p.Open(context.Background(), decode.Source{URI: "fake://tone"}, OpenOptions{
		AutoPlay: true,
		StartAt:  1500 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	waitState(t, p, events, StatePlaying, 3*time.Second)

	if snap := p.Snapshot(); snap.Position >= time.Second {
		t.Fatalf("position = %s, want playback from 0 after the seek budget ran out", snap.Position)
	}
	if snap := p.Snapshot(); snap.State == StateFailed {
		t.Fatalf("a playable track was declared failed: %s", snap.Error)
	}
}

// A read failure mid-song (CDN reset, dropped range) used to mark the whole
// track failed. It must reopen at the heard position and keep going.
func TestReadFailureReopensAtHeardPosition(t *testing.T) {
	fake := &fakeBackend{readErrOnOpen: 1, readErrAfter: 4800}
	registerFake(t, fake)

	p, err := New(testConfig("fake"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	events, cancel := p.Events(128)
	defer cancel()

	if err := p.Open(context.Background(), decode.Source{URI: "fake://tone"}, OpenOptions{AutoPlay: true}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	opened := 0
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		opened = fake.opened
		fake.mu.Unlock()
		if opened >= 2 {
			break
		}
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("event stream closed")
			}
			if ev.Kind == KindFailed {
				t.Fatalf("read failure killed the track: %s", ev.Err)
			}
		case <-time.After(10 * time.Millisecond):
		}
	}
	if opened < 2 {
		t.Fatalf("decoder opened %d times, want a reopen", opened)
	}

	waitState(t, p, events, StatePlaying, 2*time.Second)
	if snap := p.Snapshot(); snap.State == StateFailed {
		t.Fatalf("read failure killed the track: %s", snap.Error)
	}
}

func TestStaleGenerationCannotPlayOldTrack(t *testing.T) {
	fake := &fakeBackend{}
	registerFake(t, fake)

	p, err := New(testConfig("fake"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// Slow the first decoder so its chunks land after the second Open.
	first := &lateBackend{gate: make(chan struct{})}
	decode.Register(first)
	defer decode.Unregister("late")

	p.cfg.Decoder = "late"
	if err := p.Open(context.Background(), decode.Source{URI: "late://a"}, OpenOptions{AutoPlay: true}); err != nil {
		t.Fatal(err)
	}
	genA := p.Snapshot().Generation
	p.cfg.Decoder = "fake"
	if err := p.Open(context.Background(), decode.Source{URI: "fake://b"}, OpenOptions{AutoPlay: true}); err != nil {
		t.Fatal(err)
	}
	genB := p.Snapshot().Generation
	if genB <= genA {
		t.Fatalf("generation did not advance: %d -> %d", genA, genB)
	}
	close(first.gate) // let the abandoned decoder emit its late chunk

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Snapshot().State == StatePlaying {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if snap := p.Snapshot(); snap.State != StatePlaying && snap.State != StateBuffering {
		t.Fatalf("state = %s, want playing", snap.State)
	}
	if got := p.pipe.Load().Token(); got != genB {
		t.Fatalf("pipe token = %d, want %d", got, genB)
	}
}

// lateBackend blocks in Open until released, then yields one chunk per read.
type lateBackend struct {
	gate chan struct{}
	once sync.Once
}

func (b *lateBackend) Name() string  { return "late" }
func (b *lateBackend) Priority() int { return -2 }
func (b *lateBackend) Probe() (bool, string) {
	return true, "gated synthetic tone"
}

func (b *lateBackend) Open(ctx context.Context, src decode.Source, _ decode.Options) (decode.Stream, error) {
	select {
	case <-b.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	total := pcm.DefaultSampleRate * 2
	return &fakeStream{frames: total, chunkSize: 240, meta: decode.Meta{
		Codec: "late", SampleRate: pcm.DefaultSampleRate, Channels: pcm.DefaultChannels,
		Duration: 2 * time.Second, Seekable: true,
	}}, nil
}

// A load owns the previous decoder: the old decode goroutine retires without
// closing anything, so Open must take the stream away and close it. Before
// this, every skip leaked a decoder and its HTTP connection, and a skipped
// source could keep a relayed download open for the life of the process.
func TestLoadClosesPreviousStream(t *testing.T) {
	fake := &fakeBackend{}
	registerFake(t, fake)

	p, err := New(testConfig("fake"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	events, cancel := p.Events(128)
	defer cancel()

	if err := p.Open(context.Background(), decode.Source{URI: "fake://first"}, OpenOptions{AutoPlay: true}); err != nil {
		t.Fatal(err)
	}
	waitState(t, p, events, StatePlaying, 2*time.Second)
	first := fake.streamAt(0)
	if first == nil {
		t.Fatal("first stream was not opened")
	}
	if first.isClosed() {
		t.Fatal("first stream closed while it was the active one")
	}

	if err := p.Open(context.Background(), decode.Source{URI: "fake://second"}, OpenOptions{AutoPlay: true}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if first.isClosed() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("previous decoder was not closed when a new load replaced it")
}

func TestPlayerVolumeAndEQ(t *testing.T) {
	fake := &fakeBackend{}
	registerFake(t, fake)

	p, err := New(testConfig("fake"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.SetVolume(0.5)
	if p.Volume() != 0.5 {
		t.Fatalf("volume = %v", p.Volume())
	}
	p.SetMuted(true)
	if !p.Muted() {
		t.Fatal("mute not recorded")
	}
	if got := p.outGain.Linear(); got != 0 {
		t.Fatalf("muted gain = %v, want 0", got)
	}
	p.SetMuted(false)
	if got := p.outGain.Linear(); got != 0.5 {
		t.Fatalf("unmuted gain = %v, want 0.5", got)
	}
	if err := p.SetEQ([]dsp.Config{{Freq: 1000, Q: 1, Gain: 6}}); err != nil {
		t.Fatal(err)
	}
	if len(p.EQ()) != 1 {
		t.Fatalf("bands = %d", len(p.EQ()))
	}
	if st := p.DSPState(); len(st.Bands) != 1 || st.Channels != pcm.DefaultChannels {
		t.Fatalf("dsp state = %+v", st)
	}
}

// The host sets volume before Open (saved preference / transport swap). The
// async decodeLoop then derives the DSP format; the gain target must survive.
func TestPlayerVolumeSurvivesLoad(t *testing.T) {
	registerFake(t, &fakeBackend{})
	p, err := New(testConfig("fake"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetVolume(0.25)
	events, cancel := p.Events(64)
	defer cancel()
	ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	if err := p.Open(ctx, decode.Source{URI: "synthetic"}, OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	waitState(t, p, events, StateReady, 2*time.Second)
	if got := p.outGain.Linear(); math.Abs(got-0.25) > 1e-6 {
		t.Fatalf("output volume after load = %v, want 0.25", got)
	}
}

// A 12-channel source feeding a stereo endpoint must be folded in Read, not
// rejected by the decoder. This also covers the source-layout pipe swap.
func TestPlayerReadFoldsToEndpointChannels(t *testing.T) {
	registerFake(t, &fakeBackend{})
	p, err := New(testConfig("fake"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	pipe := pcm.NewPipe(4800, pcm.Format{SampleRate: pcm.DefaultSampleRate, Channels: 12})
	pipe.Swap(0)
	p.pipe.Store(pipe)
	p.outChannels.Store(pcm.DefaultChannels)
	frames := 240
	data := make([]float32, frames*12)
	for i := range data {
		data[i] = 0.1
	}
	if _, err := pipe.WriteFrames(0, data, frames); err != nil {
		t.Fatal(err)
	}
	dst := make([]float32, frames*pcm.DefaultChannels)
	if n := p.Read(dst); n != frames {
		t.Fatalf("read frames = %d, want %d", n, frames)
	}
	var nonzero bool
	for _, v := range dst {
		if v > 1 || v < -1 {
			t.Fatalf("folded sample out of range: %v", v)
		}
		if v != 0 {
			nonzero = true
		}
	}
	if !nonzero {
		t.Fatal("fold produced silence")
	}
}

// The ring can hold seconds of audio. Volume is applied after it, so a slider
// move must affect frames that were already buffered — that is the whole point
// of the consumer-side gain.
func TestPlayerReadAppliesVolumeToBufferedFrames(t *testing.T) {
	registerFake(t, &fakeBackend{})
	p, err := New(testConfig("fake"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	pipe := pcm.NewPipe(4800, pcm.DefaultFormat())
	pipe.Swap(0)
	p.pipe.Store(pipe)
	p.outChannels.Store(pcm.DefaultChannels)
	frames := 120
	data := make([]float32, frames*2)
	for i := range data {
		data[i] = 0.8
	}
	if _, err := pipe.WriteFrames(0, data, frames); err != nil {
		t.Fatal(err)
	}
	p.SetVolume(0.25)
	p.outGain.SetRamp(0)
	p.outGain.Snap()
	dst := make([]float32, frames*2)
	if n := p.Read(dst); n != frames {
		t.Fatalf("read frames = %d, want %d", n, frames)
	}
	for i, v := range dst {
		if math.Abs(float64(v)-0.2) > 1e-5 {
			t.Fatalf("sample %d = %v, want 0.2 (buffered frame not attenuated)", i, v)
		}
	}
}

func TestPlayerPauseResumeAndStop(t *testing.T) {
	fake := &fakeBackend{}
	registerFake(t, fake)

	p, err := New(testConfig("fake"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	events, cancel := p.Events(128)
	defer cancel()

	if err := p.Open(context.Background(), decode.Source{URI: "fake://tone"}, OpenOptions{AutoPlay: true}); err != nil {
		t.Fatal(err)
	}
	waitState(t, p, events, StatePlaying, 2*time.Second)
	// Let the clock get a real reading before pausing.
	deadlineRun := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadlineRun) && p.Snapshot().Position < 100*time.Millisecond {
		time.Sleep(5 * time.Millisecond)
	}
	if err := p.Pause(); err != nil {
		t.Fatal(err)
	}
	if got := p.Snapshot().State; got != StatePaused {
		t.Fatalf("state = %s, want paused", got)
	}
	time.Sleep(100 * time.Millisecond) // let in-flight callbacks settle
	before := p.Snapshot().Position
	time.Sleep(200 * time.Millisecond)
	if after := p.Snapshot().Position; after != before {
		t.Fatalf("position advanced while paused: %s -> %s", before, after)
	}
	if err := p.Play(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && p.Snapshot().Position == before {
		time.Sleep(5 * time.Millisecond)
	}
	if p.Snapshot().Position <= before {
		t.Fatal("position did not resume")
	}
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	if got := p.Snapshot().State; got != StateStopped {
		t.Fatalf("state after stop = %s", got)
	}
}

func TestPlayerOpenFailureIsReported(t *testing.T) {
	fake := &fakeBackend{failErr: errors.New("broken container")}
	registerFake(t, fake)

	p, err := New(testConfig("fake"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	events, cancel := p.Events(16)
	defer cancel()

	if err := p.Open(context.Background(), decode.Source{URI: "fake://bad"}, OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-events:
		if ev.Kind != KindFailed {
			t.Fatalf("event = %s, want failed", ev.Kind)
		}
		if ev.Err == "" {
			t.Fatal("failed event without message")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no failure event")
	}
	if got := p.Snapshot().State; got != StateFailed {
		t.Fatalf("state = %s, want failed", got)
	}
}

func TestDeviceWatchRecoversFromLostEndpoint(t *testing.T) {
	fake := &fakeBackend{}
	registerFake(t, fake)

	watch := &flakyDriver{DeviceStatus: output.Device{ID: "a", Driver: "flaky", Name: "A", Default: true}}
	output.Register(watch)
	defer output.Unregister("flaky")

	cfg := testConfig("fake")
	cfg.Driver = "flaky"
	cfg.DevicePolling = 20 * time.Millisecond
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	events, cancel := p.Events(64)
	defer cancel()

	if err := p.Open(context.Background(), decode.Source{URI: "fake://tone"}, OpenOptions{AutoPlay: true}); err != nil {
		t.Fatal(err)
	}
	waitState(t, p, events, StatePlaying, 2*time.Second)

	// Pull the rug: the endpoint disappears from the driver's list.
	watch.mu.Lock()
	watch.DeviceStatus = output.Device{ID: "b", Driver: "flaky", Name: "B", Default: true}
	watch.mu.Unlock()

	deadline := time.Now().Add(3 * time.Second)
	sawLost := false
	for time.Now().Before(deadline) {
		select {
		case ev := <-events:
			switch ev.Kind {
			case KindDeviceLost:
				sawLost = true
			case KindPlaying:
				if sawLost {
					return
				}
			case KindFailed:
				t.Fatalf("player failed: %s", ev.Err)
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatalf("device lost=%v state=%s", sawLost, p.Snapshot().State)
}

// flakyDriver is a null-like driver whose endpoint list can change under the
// player, to exercise the device watcher.
type flakyDriver struct {
	mu           sync.Mutex
	DeviceStatus output.Device
	session      *flakySession
}

func (d *flakyDriver) Name() string  { return "flaky" }
func (d *flakyDriver) Priority() int { return 50 }
func (d *flakyDriver) Probe() (bool, string) {
	return true, "test driver"
}

func (d *flakyDriver) Devices() ([]output.Device, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return []output.Device{d.DeviceStatus}, nil
}

func (d *flakyDriver) Open(opts output.Options) (output.Output, error) {
	s := &flakySession{pump: output.NewPump(opts.Source, opts.Format, opts.OnUnderrun, opts.OnError, opts.Log), period: opts.PeriodFrames, format: opts.Format}
	d.mu.Lock()
	d.session = s
	s.device = d.DeviceStatus
	d.mu.Unlock()
	return s, nil
}

type flakySession struct {
	pump   *output.Pump
	period int
	format pcm.Format

	mu     sync.Mutex
	device output.Device
	state  output.State
	done   chan struct{}
}

func (s *flakySession) Start() error {
	s.mu.Lock()
	if s.state == output.StateRunning {
		s.mu.Unlock()
		return nil
	}
	s.state = output.StateRunning
	s.done = make(chan struct{})
	device, period, format := s.device, s.period, s.format
	s.mu.Unlock()

	go func() {
		tick := time.NewTicker(time.Duration(period) * time.Second / time.Duration(format.SampleRate))
		defer tick.Stop()
		buf := make([]float32, period*format.Channels)
		for {
			select {
			case <-s.done:
				return
			case <-tick.C:
				s.mu.Lock()
				running := s.state == output.StateRunning
				s.mu.Unlock()
				if running {
					s.pump.Fill(buf, period)
				}
			}
		}
	}()
	_ = device
	return nil
}

func (s *flakySession) Pause() error  { return nil }
func (s *flakySession) Resume() error { return nil }

func (s *flakySession) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == output.StateRunning {
		s.state = output.StateStopped
		close(s.done)
	}
	return nil
}

func (s *flakySession) Close() error {
	if err := s.Stop(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = output.StateClosed
	return nil
}

func (s *flakySession) State() output.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *flakySession) Device() output.Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.device
}

func (s *flakySession) Format() pcm.Format { return s.format }

func (s *flakySession) FramesWritten() uint64 { w, _ := s.pump.Counters(); return w }
func (s *flakySession) Underruns() uint64     { _, u := s.pump.Counters(); return u }
func (s *flakySession) Latency() time.Duration {
	return time.Duration(s.period) * time.Second / time.Duration(s.format.SampleRate)
}
