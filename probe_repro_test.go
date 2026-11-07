package liino

import (
	"context"
	"io"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/team-quaver/liino/decode"
	"github.com/team-quaver/liino/pcm"
)

// probeBackend is a synthetic decoder whose track length a probe can pick.
type probeBackend struct {
	mu    sync.Mutex
	name  string
	total time.Duration
	opened int
}

func (b *probeBackend) Name() string  { return b.name }
func (b *probeBackend) Priority() int { return -1 }
func (b *probeBackend) Probe() (bool, string) {
	return true, "synthetic tone"
}

func (b *probeBackend) Open(context.Context, decode.Source, decode.Options) (decode.Stream, error) {
	b.mu.Lock()
	b.opened++
	b.mu.Unlock()
	return &probeStream{frames: int(b.total.Seconds() * float64(pcm.DefaultSampleRate))}, nil
}

type probeStream struct {
	mu       sync.Mutex
	frames   int
	produced int
	closed   bool
}

func (s *probeStream) Format() pcm.Format { return pcm.DefaultFormat() }
func (s *probeStream) Meta() decode.Meta {
	return decode.Meta{
		Codec: "probe", SampleRate: pcm.DefaultSampleRate, Channels: pcm.DefaultChannels,
		Duration: time.Duration(s.frames) * time.Second / time.Duration(pcm.DefaultSampleRate),
		Seekable: true,
	}
}

func (s *probeStream) Read() (pcm.Chunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return pcm.Chunk{}, decode.ErrNotOpen
	}
	if s.produced >= s.frames {
		return pcm.Chunk{}, io.EOF
	}
	n := min(pcm.ChunkFrames, s.frames-s.produced)
	out := make([]float32, n*pcm.DefaultChannels)
	for i := 0; i < n; i++ {
		t := float64(s.produced+i) / pcm.DefaultSampleRate
		v := float32(0.2 * math.Sin(2*math.Pi*220*t))
		out[i*2], out[i*2+1] = v, v
	}
	s.produced += n
	return pcm.Chunk{Data: out, Frames: n, Format: pcm.DefaultFormat()}, nil
}

func (s *probeStream) Seek(d time.Duration) (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.produced = int(d.Seconds() * float64(pcm.DefaultSampleRate))
	return d, nil
}

func (s *probeStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

type probeLog struct {
	mu sync.Mutex
	db []string
}

func (l *probeLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.db = append(l.db, s)
}

func (l *probeLog) dump(t *testing.T) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.db {
		t.Log(s)
	}
}

func probeConfig(name string, total time.Duration, l *probeLog) Config {
	cfg := testConfig(name)
	cfg.Log = func(s string) { l.add(time.Now().Format("15:04:05.000 ") + s) }
	return cfg
}

// TestProbeStuckAfterEnd plays several tracks back to back the way the host
// does (open autoplay, wait for ended, open the next one) and reports the
// first cycle where the timeline stops moving.
func TestProbeStuckAfterEnd(t *testing.T) {
	for round := 0; round < 30; round++ {
		b := &probeBackend{name: "probe", total: 400 * time.Millisecond}
		decode.Register(b)
		logs := &probeLog{}
		p, err := New(probeConfig("probe", 400*time.Millisecond, logs))
		if err != nil {
			t.Fatal(err)
		}
		events, cancel := p.Events(256)
		got := make(chan Event, 256)
		go func() {
			for ev := range events {
				got <- ev
			}
		}()

		stuck := false
		for cycle := 0; cycle < 4 && !stuck; cycle++ {
			if err := p.Open(context.Background(), decode.Source{URI: "probe://tone"}, OpenOptions{AutoPlay: true}); err != nil {
				t.Fatal(err)
			}
			if !waitFor(t, p, got, StateEnded, 3*time.Second) {
				stuck = true
			}
		}
		if stuck {
			snap := p.Snapshot()
			t.Fatalf("round %d wedged at cycle end: %+v", round, snap)
		}
		cancel()
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		decode.Unregister("probe")
		if t.Failed() {
			logs.dump(t)
			return
		}
	}
}

func waitFor(t *testing.T, p *Player, ch <-chan Event, want State, timeout time.Duration) bool {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if p.State() == want {
			return true
		}
		select {
		case ev := <-ch:
			if ev.Kind == KindFailed {
				t.Logf("event failed: %s", ev.Err)
			}
		case <-time.After(2 * time.Millisecond):
		case <-deadline:
			return false
		}
	}
}

// report dumps whatever explains a wedged engine: the state machine's own
// fields plus the pipe and device behind them.
func report(t *testing.T, where string, p *Player) bool {
	t.Helper()
	p.mu.Lock()
	eof, want, alive, closed := p.eof, p.wantPlay, p.loopAlive, p.closed
	loadGen, seekGen := p.gen.curLoad(), p.gen.curSeek()
	p.mu.Unlock()
	pipe := p.pipe.Load()
	occ, capacity, tok, dropped := pipe.Available(), pipe.Capacity(), pipe.Token(), pipe.Dropped()
	p.mu.Lock()
	out := p.out
	p.mu.Unlock()
	dev := "nil"
	if out != nil {
		dev = out.State().String()
	}
	t.Logf("WEDGE %s: state=%s wantplay=%v eof=%v loopAlive=%v closed=%v loadGen=%d seekGen=%d pulled=%d decoded=%d pipe{frames=%d cap=%d token=%d dropped=%d} device=%s snapshot=%+v",
		where, p.State(), want, eof, alive, closed, loadGen, seekGen, p.pulled.Load(), p.decoded.Load(),
		occ, capacity, tok, dropped, dev, p.Snapshot())
	return false
}

// TestProbeSeekDragWedges runs the two user-visible reproducers over and over:
// a drag across the progress bar while playing, and a drag right after the
// track ended. A round fails as soon as the timeline stops moving.
func TestProbeSeekDragWedges(t *testing.T) {
	for round := 0; round < 40; round++ {
		b := &fakeBackend{}
		registerFake(t, b)
		logs := &probeLog{}
		cfg := testConfig("fake")
		cfg.Log = func(s string) { logs.add(time.Now().Format("15:04:05.000 ") + s) }
		p, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		events, cancel := p.Events(256)
		got := make(chan Event, 512)
		go func() {
			for ev := range events {
				select {
				case got <- ev:
				default:
				}
			}
		}()

		seekTo := func(d time.Duration) {
			if err := p.Seek(d); err != nil {
				t.Fatalf("seek: %v", err)
			}
		}
		progress := func(where string) bool {
			before := p.pulled.Load()
			time.Sleep(250 * time.Millisecond)
			if p.pulled.Load() <= before {
				return report(t, where, p)
			}
			return true
		}

		if err := p.Open(context.Background(), decode.Source{URI: "probe://a"}, OpenOptions{AutoPlay: true}); err != nil {
			t.Fatal(err)
		}
		if !waitFor(t, p, got, StatePlaying, 2*time.Second) {
			report(t, "round A open", p)
			logs.dump(t)
			cancel()
			_ = p.Close()
			return
		}
		for _, at := range []time.Duration{1500 * time.Millisecond, 300 * time.Millisecond, 1900 * time.Millisecond} {
			seekTo(at)
			time.Sleep(40 * time.Millisecond)
		}
		if !progress("round A after drag") {
			logs.dump(t)
			cancel()
			_ = p.Close()
			return
		}
		// Land on the end, then drag the bar again: the loop has retired here.
		if !waitFor(t, p, got, StateEnded, 4*time.Second) {
			report(t, "round A wait ended", p)
			logs.dump(t)
			cancel()
			_ = p.Close()
			return
		}
		seekTo(900 * time.Millisecond)
		if !progress("round A seek after ended") {
			logs.dump(t)
			cancel()
			_ = p.Close()
			return
		}
		cancel()
		_ = p.Close()
	}
}
