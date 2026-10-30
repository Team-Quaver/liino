package liino

import (
	"fmt"
	"sync/atomic"
	"time"
)

// State is the playback lifecycle of a Player.
type State int

// Lifecycle states.
const (
	// StateIdle means no media is loaded.
	StateIdle State = iota
	// StateLoading means the decoder is opening the source.
	StateLoading
	// StateReady means the source opened and preroll is in progress.
	StateReady
	// StatePlaying means audio is reaching the device.
	StatePlaying
	// StatePaused means the device session is alive and emitting silence.
	StatePaused
	// StateBuffering means playback stalled waiting for data.
	StateBuffering
	// StateEnded means the stream drained after end of file.
	StateEnded
	// StateStopped means playback stopped and the source was unloaded.
	StateStopped
	// StateFailed means the last operation left the player unusable.
	StateFailed
	// StateClosed means the player released all resources.
	StateClosed
)

// String names the state for logs and JSON.
func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateLoading:
		return "loading"
	case StateReady:
		return "ready"
	case StatePlaying:
		return "playing"
	case StateBuffering:
		return "buffering"
	case StatePaused:
		return "paused"
	case StateEnded:
		return "ended"
	case StateStopped:
		return "stopped"
	case StateFailed:
		return "failed"
	case StateClosed:
		return "closed"
	default:
		return fmt.Sprintf("state(%d)", int(s))
	}
}

// Live reports whether the state can produce sound.
func (s State) Live() bool {
	return s == StatePlaying || s == StateBuffering || s == StateReady
}

// generations are the only way late callbacks get rejected. Each counter is
// monotonic and never resets, so a stale event can always be recognised.
type generations struct {
	load  atomic.Uint64 // bumped by Open/Stop: which media owns the pipeline
	seek  atomic.Uint64 // bumped by Seek: which position the pipeline plays
	epoch atomic.Uint64 // bumped when the output session is rebuilt
}

func (g *generations) nextLoad() uint64  { return g.load.Add(1) }
func (g *generations) nextSeek() uint64  { return g.seek.Add(1) }
func (g *generations) nextEpoch() uint64 { return g.epoch.Add(1) }
func (g *generations) curLoad() uint64   { return g.load.Load() }
func (g *generations) curSeek() uint64   { return g.seek.Load() }
func (g *generations) curEpoch() uint64  { return g.epoch.Load() }

// clock turns frame counts into media time.
type clock struct {
	// base is the position of the first frame after the last seek or load.
	base time.Duration
	rate int
}

func newClock(rate int) *clock {
	if rate <= 0 {
		rate = 48000
	}
	return &clock{rate: rate}
}

func (c *clock) frames(d time.Duration) uint64 {
	return uint64(d.Seconds() * float64(c.rate))
}

func (c *clock) position(pulled uint64) time.Duration {
	return c.base + time.Duration(float64(pulled)/float64(c.rate)*float64(time.Second))
}

func (c *clock) rebase(pos time.Duration, pulled uint64) {
	c.base = pos - time.Duration(float64(pulled)/float64(c.rate)*float64(time.Second))
}
