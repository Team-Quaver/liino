//go:build linux

// Package pipewire outputs PCM straight into the PipeWire session bus.
//
// The binding is github.com/bnema/purego-pipewire: pure Go over purego, so no
// CGO and no libpulse. Liino deliberately does not use the ALSA path here -
// Quaver runs inside desktop sessions where PipeWire or its PulseAudio
// compatibility layer owns the sound card, and opening /dev/snd directly would
// either fail with EBUSY or steal the device from the session.
package pipewire

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	pw "github.com/bnema/purego-pipewire/pipewire"

	"github.com/team-quaver/liino/output"
	"github.com/team-quaver/liino/pcm"
)

// DefaultID is the only endpoint this driver can address today: whatever
// node PipeWire's session manager picks as the default sink.
const DefaultID = "pipewire/default"

type driver struct{}

func (driver) Name() string  { return "pipewire" }
func (driver) Priority() int { return 0 }

var (
	rtOnce sync.Once
	rt     pw.Runtime
	rtErr  error
)

// runtime initialises the PipeWire library once per process.
func runtime() (pw.Runtime, error) {
	rtOnce.Do(func() {
		rt, rtErr = pw.Init()
	})
	if rtErr != nil {
		return nil, fmt.Errorf("%w: %v", output.ErrUnavailable, rtErr)
	}
	return rt, nil
}

func (driver) Probe() (bool, string) {
	if _, err := runtime(); err != nil {
		return false, err.Error()
	}
	return true, "PipeWire session bus"
}

// Devices reports the session default sink. Selecting an individual PipeWire
// node needs stream "node.target" plumbing that upstream's Player does not
// expose yet; see the driver notes in README.md.
func (driver) Devices() ([]output.Device, error) {
	if _, err := runtime(); err != nil {
		return nil, err
	}
	return []output.Device{{
		ID:      DefaultID,
		Driver:  "pipewire",
		Name:    "PipeWire default sink",
		Default: true,
		Native:  "session manager selection",
		Formats: []pcm.Format{pcm.DefaultFormat()},
	}}, nil
}

func (driver) Open(opts output.Options) (output.Output, error) {
	if _, err := runtime(); err != nil {
		return nil, err
	}
	if opts.DeviceID != "" && opts.DeviceID != DefaultID {
		return nil, fmt.Errorf("%w: %s (PipeWire targets the session default sink)",
			output.ErrNoSuchDevice, opts.DeviceID)
	}

	s := &session{
		format: opts.Format,
		period: opts.PeriodFrames,
		device: output.Device{ID: DefaultID, Driver: "pipewire", Name: "PipeWire default sink", Default: true},
	}
	s.pump = output.NewPump(opts.Source, opts.Format, opts.OnUnderrun, opts.OnError, opts.Log)

	player, err := pw.NewPlayer(pw.PlayerConfig{
		SampleRate:      opts.Format.SampleRate,
		Channels:        opts.Format.Channels,
		FramesPerBuffer: opts.PeriodFrames,
		SampleFormat:    pw.SampleFormatF32,
		UnderrunPolicy:  pw.UnderrunFillSilence,
	}, pw.PlayerCallbacks{
		Fill:       s.fill,
		OnUnderrun: func(frames int) { s.Logf("graph underrun, %d frames filled with silence", frames) },
		OnDrain:    func() { s.Logf("stream drained") },
		OnError:    func(err error) { s.ReportError(fmt.Errorf("pipewire: %w", err)) },
		OnStateChange: func(state pw.PlayerState) {
			s.Logf("player state %s", stateName(state))
		},
	})
	if err != nil {
		return nil, err
	}
	s.player = player
	s.Logf("stream created: %d Hz / %d ch / %d frames per buffer",
		opts.Format.SampleRate, opts.Format.Channels, opts.PeriodFrames)
	return s, nil
}

type session struct {
	player pw.Player
	pump   *output.Pump
	format pcm.Format
	period int
	device output.Device

	mu     sync.Mutex
	state  output.State
	paused atomic.Bool
}

// fill runs on the PipeWire loop thread: copy frames into the planar graph
// buffers, never block, never allocate.
func (s *session) fill(buf *pw.PCMBuffer) (int, error) {
	if buf == nil || buf.Frames <= 0 {
		return 0, nil
	}
	if s.paused.Load() || s.State() != output.StateRunning {
		for _, row := range buf.Samples {
			clear(row)
		}
		return buf.Frames, nil
	}
	return s.pump.FillPlanar(buf.Samples, buf.Frames, buf.Stride), nil
}

func (s *session) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == output.StateClosed {
		return output.ErrClosed
	}
	if s.state == output.StateRunning {
		return nil
	}
	s.paused.Store(false)
	if err := s.player.Start(); err != nil {
		return err
	}
	s.state = output.StateRunning
	return nil
}

// Pause keeps the stream active and feeds silence: the graph stays scheduled,
// so there is no click or renegotiation on resume.
func (s *session) Pause() error {
	if s.State() == output.StateClosed {
		return output.ErrClosed
	}
	s.paused.Store(true)
	return nil
}

func (s *session) Resume() error {
	if s.State() != output.StateRunning {
		return s.Start()
	}
	s.paused.Store(false)
	return nil
}

func (s *session) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopLocked()
}

func (s *session) stopLocked() error {
	if s.state == output.StateClosed {
		return output.ErrClosed
	}
	s.paused.Store(true)
	if s.state == output.StateRunning {
		if err := s.player.Stop(); err != nil {
			return err
		}
	}
	s.state = output.StateStopped
	return nil
}

func (s *session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == output.StateClosed {
		return nil
	}
	err := s.player.Close()
	s.state = output.StateClosed
	return err
}

func (s *session) State() output.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *session) Device() output.Device { return s.device }
func (s *session) Format() pcm.Format    { return s.format }

func (s *session) FramesWritten() uint64 { w, _ := s.pump.Counters(); return w }
func (s *session) Underruns() uint64     { _, u := s.pump.Counters(); return u }

func (s *session) Latency() time.Duration {
	return time.Duration(s.period) * time.Second / time.Duration(s.format.SampleRate)
}

func (s *session) Reset() { s.pump.Reset() }

func (s *session) ReportError(err error) {
	if err != nil && !errors.Is(err, output.ErrClosed) {
		s.pump.ReportError(err)
	}
}

func (s *session) Logf(format string, args ...any) { s.pump.Logf("[pipewire] "+format, args...) }

func stateName(st pw.PlayerState) string {
	switch st {
	case pw.PlayerStateIdle:
		return "idle"
	case pw.PlayerStateStarting:
		return "starting"
	case pw.PlayerStatePlaying:
		return "playing"
	case pw.PlayerStatePaused:
		return "paused"
	case pw.PlayerStateStopped:
		return "stopped"
	case pw.PlayerStateClosing:
		return "closing"
	case pw.PlayerStateClosed:
		return "closed"
	case pw.PlayerStateError:
		return "error"
	default:
		return fmt.Sprintf("state(%d)", int(st))
	}
}

func init() { output.Register(driver{}) }
