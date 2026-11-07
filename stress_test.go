package liino

import (
	"context"
	"testing"
	"time"

	"github.com/team-quaver/liino/decode"
)

// TestStressEndToEndCycles hammers the transitions users report as fatal:
// track after track, drags across the bar, drags at the very tail, a scrub
// right after the track ended, pause/scrub/play and a device rescan. A round
// fails as soon as the device stops consuming while the engine claims to be
// running — the definition of "it froze and nothing recovers it".
//
// It is a loop on purpose: every wedge found so far needed the pipeline to be
// parked by the *previous* cycle. Run it alone with `go test -run TestStress`.
func TestStressEndToEndCycles(t *testing.T) {
	if testing.Short() {
		t.Skip("stress: skip under -short")
	}
	rounds, cycles := 8, 3
	for round := 0; round < rounds; round++ {
		b := &fakeBackend{}
		if round%4 == 1 {
			b.unseekable = true
		}
		registerFake(t, b)

		cfg := testConfig("fake")
		cfg.FadeOut = 120 * time.Millisecond
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

		silent := func(where string, wait time.Duration) bool {
			before := p.pulled.Load()
			time.Sleep(wait)
			if p.pulled.Load() > before {
				return false
			}
			snap := p.Snapshot()
			switch snap.State {
			case StatePaused, StateStopped, StateEnded, StateIdle, StateFailed:
				return false // deliberate silence, and Failed is reported by the waits below
			}
			intent, want, alive, atEOF := p.playFlags()
			t.Errorf("round %d %s: silent — state=%s position=%s frames=%d pulled %d->%d intent=%v wantplay=%v loopAlive=%v eof=%v",
				round, where, snap.State, snap.Position, snap.FramesWritten, before, p.pulled.Load(),
				intent, want, alive, atEOF)
			return true
		}

		bad := false
		for cycle := 0; cycle < cycles && !bad; cycle++ {
			uri := "fake://a"
			if cycle == 1 {
				uri = "fake://b"
			}
			if err := p.Open(context.Background(), decode.Source{URI: uri}, OpenOptions{AutoPlay: true}); err != nil {
				t.Fatal(err)
			}
			if !waitEngine(p, got, StatePlaying, 3*time.Second) {
				t.Errorf("round %d cycle %d never started playing: %s", round, cycle, p.Snapshot().State)
				bad = true
				break
			}
			for _, at := range []time.Duration{1400 * time.Millisecond, 300 * time.Millisecond, 1980 * time.Millisecond} {
				if err := p.Seek(at); err != nil {
					t.Fatal(err)
				}
				time.Sleep(35 * time.Millisecond)
			}
			if bad = silent("after drag", 300*time.Millisecond); bad {
				break
			}
			if !waitEngine(p, got, StateEnded, 4*time.Second) {
				t.Errorf("round %d cycle %d never ended: %s", round, cycle, p.Snapshot().State)
				bad = true
				break
			}
			if err := p.Seek(600 * time.Millisecond); err != nil {
				t.Fatal(err)
			}
			if bad = silent("seek after ended", 400*time.Millisecond); bad {
				break
			}
			_ = p.Pause()
			_ = p.Seek(200 * time.Millisecond)
			time.Sleep(80 * time.Millisecond)
			_ = p.Play()
			if bad = silent("play after paused scrub", 400*time.Millisecond); bad {
				break
			}
			_ = p.SetDevice("")
			bad = silent("after device rescan", 400*time.Millisecond)
		}
		cancel()
		_ = p.Close()
		decode.Unregister("fake")
		if bad || t.Failed() {
			return
		}
	}
}

// waitEngine polls the lifecycle until the wanted state shows up, ignoring the
// event channel except to surface failures early.
func waitEngine(p *Player, ch <-chan Event, want State, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		if p.State() == want {
			return true
		}
		select {
		case ev := <-ch:
			if ev.Kind == KindFailed {
				return false
			}
		case <-time.After(2 * time.Millisecond):
		case <-deadline:
			return false
		}
	}
}

// playFlags reads the private play bookkeeping for the stress failure message.
func (p *Player) playFlags() (playIntent, wantPlay, loopAlive, eof bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.playIntent, p.wantPlay, p.loopAlive, p.eof
}
