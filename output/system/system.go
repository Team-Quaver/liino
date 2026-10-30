//go:build windows || darwin

// Package system drives the in-process WASAPI (Windows) and Core Audio
// (macOS) backends of github.com/vcaesar/audio-go.
//
// audio-go reaches those APIs through syscalls and COM vtables only: no CGO,
// no miniaudio, no helper DLL. The render session therefore belongs to this
// process, which is what keeps Windows application-audio capture (Discord, OBS,
// Kook, ...) pointed at Quaver Music alone, and shared mode is what makes
// loopback possible at all.
package system

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	audiogo "github.com/vcaesar/audio-go"

	"github.com/team-quaver/liino/output"
	"github.com/team-quaver/liino/pcm"
)

// driver wraps one audio-go backend.
type driver struct {
	name     string
	kind     audiogo.Backend
	priority int
}

func (d *driver) Name() string  { return d.name }
func (d *driver) Priority() int { return d.priority }

var (
	ctxMu sync.Mutex
	ctxs  = map[audiogo.Backend]*audiogo.AllocatedContext{}
)

// sharedContext returns the process-wide context for a backend. Reopening a
// context mid-playback causes COM churn, so one is cached per backend for the
// process lifetime and released by Shutdown.
func sharedContext(kind audiogo.Backend, logf func(string)) (*audiogo.AllocatedContext, error) {
	ctxMu.Lock()
	defer ctxMu.Unlock()
	if ac, ok := ctxs[kind]; ok {
		return ac, nil
	}
	var proc audiogo.LogProc
	if logf != nil {
		proc = func(msg string) { logf("audio-go: " + msg) }
	}
	ac, err := audiogo.InitContext([]audiogo.Backend{kind}, audiogo.ContextConfig{}, proc)
	if err != nil {
		return nil, err
	}
	if got := ac.Backend(); got != kind {
		_ = ac.Uninit()
		ac.Free()
		return nil, fmt.Errorf("output system: wanted %s, audio-go selected %s", kind, got)
	}
	ctxs[kind] = ac
	return ac, nil
}

// Shutdown releases every cached context. Call it on process teardown only.
func Shutdown() {
	ctxMu.Lock()
	defer ctxMu.Unlock()
	for kind, ac := range ctxs {
		_ = ac.Uninit()
		ac.Free()
		delete(ctxs, kind)
	}
}

func (d *driver) Probe() (bool, string) {
	ac, err := sharedContext(d.kind, nil)
	if err != nil {
		return false, err.Error()
	}
	return true, "audio-go " + ac.Backend().String()
}

// Devices lists render endpoints.
func (d *driver) Devices() ([]output.Device, error) {
	ac, err := sharedContext(d.kind, nil)
	if err != nil {
		return nil, err
	}
	raw, err := ac.Devices(audiogo.Playback)
	if err != nil {
		return nil, err
	}
	out := make([]output.Device, 0, len(raw))
	for _, dev := range raw {
		id := dev.ID.String()
		out = append(out, output.Device{
			ID:      id,
			Driver:  d.name,
			Name:    fallback(dev.Name(), id),
			Default: dev.IsDefault != 0,
			Native:  id,
			Formats: nativeFormats(dev.Formats),
		})
	}
	return out, nil
}

func nativeFormats(formats []audiogo.DataFormat) []pcm.Format {
	if len(formats) == 0 {
		return nil
	}
	out := make([]pcm.Format, 0, len(formats))
	for _, f := range formats {
		out = append(out, pcm.Format{SampleRate: int(f.SampleRate), Channels: int(f.Channels)})
	}
	return out
}

func fallback(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Open creates a render session on the requested, or default, endpoint.
func (d *driver) Open(opts output.Options) (output.Output, error) {
	ac, err := sharedContext(d.kind, opts.Log)
	if err != nil {
		return nil, err
	}

	cfg := audiogo.DefaultDeviceConfig(audiogo.Playback)
	cfg.DeviceType = audiogo.Playback
	cfg.SampleRate = uint32(opts.Format.SampleRate)
	cfg.PeriodSizeInFrames = uint32(opts.PeriodFrames)
	cfg.Periods = 3
	cfg.PerformanceProfile = audiogo.LowLatency
	cfg.Playback = audiogo.SubConfig{
		Format:    audiogo.FormatF32,
		Channels:  uint32(opts.Format.Channels),
		ShareMode: audiogo.Shared, // never exclusive: loopback capture requires shared mode
	}
	if opts.DeviceID != "" {
		id, err := decodeDeviceID(opts.DeviceID)
		if err != nil {
			return nil, err
		}
		cfg.Playback.DeviceID = id.Pointer()
	}

	s := &session{
		name:   d.name,
		format: opts.Format,
		period: opts.PeriodFrames,
	}
	s.pump = output.NewPump(opts.Source, opts.Format, opts.OnUnderrun, opts.OnError, opts.Log)

	dev, err := audiogo.InitDevice(ac.Context, cfg, audiogo.DeviceCallbacks{
		Data: s.data,
		Stop: func() { s.Logf("stream stopped") },
	})
	if err != nil {
		return nil, err
	}
	s.dev = dev
	s.device = output.Device{
		ID:      opts.DeviceID,
		Driver:  d.name,
		Name:    deviceName(ac, opts.DeviceID),
		Native:  opts.DeviceID,
		Formats: []pcm.Format{opts.Format},
	}
	s.Logf("session ready on %q: %d Hz / %d ch / %d frames per period",
		s.device.Name, dev.PlaybackInternalSampleRate(), dev.PlaybackInternalChannels(), opts.PeriodFrames)
	return s, nil
}

func deviceName(ac *audiogo.AllocatedContext, id string) string {
	if id == "" {
		return "Default output"
	}
	raw, err := ac.Devices(audiogo.Playback)
	if err != nil {
		return id
	}
	for _, dev := range raw {
		if dev.ID.String() == id {
			return fallback(dev.Name(), id)
		}
	}
	return id
}

// decodeDeviceID turns the opaque hex id back into an audio-go DeviceID.
func decodeDeviceID(s string) (audiogo.DeviceID, error) {
	var id audiogo.DeviceID
	raw, err := hex.DecodeString(s)
	if err != nil {
		return id, fmt.Errorf("output system: device id %q: %w", s, err)
	}
	if len(raw) > len(id) {
		return id, fmt.Errorf("output system: device id %q too long", s)
	}
	copy(id[:], raw)
	return id, nil
}

// session is one open render stream.
type session struct {
	name   string
	dev    *audiogo.Device
	pump   *output.Pump
	format pcm.Format
	period int
	device output.Device

	mu      sync.Mutex
	state   output.State
	scratch []float32
	paused  atomic.Bool
}

// data runs on the OS audio thread: no allocation, no blocking, no logging.
func (s *session) data(out, _ []byte, frames uint32) {
	n := int(frames)
	if n == 0 {
		return
	}
	if s.paused.Load() || s.State() != output.StateRunning {
		clear(out)
		return
	}
	ch := s.format.Channels
	if dst, ok := floatView(out, n*ch); ok {
		s.pump.Fill(dst, n)
		return
	}
	// Unaligned host buffer (rare): stage in Go memory, then copy bytes.
	s.scratch = ensure(s.scratch, n*ch)
	s.pump.Fill(s.scratch[:n*ch], n)
	copy(out, pcm.F32Bytes(s.scratch[:n*ch]))
}

// Start begins pulling audio.
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
	if err := s.dev.Start(); err != nil {
		return err
	}
	s.state = output.StateRunning
	return nil
}

// Pause keeps the device session alive and emits silence, so resuming has no
// ramp-up and the endpoint is never released mid-playback.
func (s *session) Pause() error {
	if s.State() == output.StateClosed {
		return output.ErrClosed
	}
	s.paused.Store(true)
	return nil
}

// Resume undoes Pause, restarting the stream if it was stopped.
func (s *session) Resume() error {
	if s.State() != output.StateRunning {
		return s.Start()
	}
	s.paused.Store(false)
	return nil
}

// Stop halts the stream but keeps the device initialised for a later Start.
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
		if err := s.dev.Stop(); err != nil && !errors.Is(err, audiogo.ErrDeviceNotInitialized) {
			return err
		}
	}
	s.state = output.StateStopped
	return nil
}

// Close releases the device. The shared context stays for the next session.
func (s *session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == output.StateClosed {
		return nil
	}
	if s.state == output.StateRunning {
		if err := s.dev.Stop(); err != nil && !errors.Is(err, audiogo.ErrDeviceNotInitialized) {
			s.Logf("stop before close: %v", err)
		}
	}
	s.dev.Uninit()
	s.state = output.StateClosed
	return nil
}

// State reports the session lifecycle.
func (s *session) State() output.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Device returns the endpoint the session opened.
func (s *session) Device() output.Device { return s.device }

// Format returns the negotiated pipeline format.
func (s *session) Format() pcm.Format { return s.format }

// FramesWritten counts real audio frames handed to the device.
func (s *session) FramesWritten() uint64 { w, _ := s.pump.Counters(); return w }

// Underruns counts the callback gaps.
func (s *session) Underruns() uint64 { _, u := s.pump.Counters(); return u }

// Latency is the configured buffer depth: three periods.
func (s *session) Latency() time.Duration {
	return 3 * time.Duration(s.period) * time.Second / time.Duration(s.format.SampleRate)
}

// Reset clears counters and source state after a seek or device switch.
func (s *session) Reset() { s.pump.Reset() }

// ReportError forwards an async failure to the owner.
func (s *session) ReportError(err error) { s.pump.ReportError(err) }

// Logf writes driver diagnostics through the configured sink.
func (s *session) Logf(format string, args ...any) {
	s.pump.Logf("["+s.name+"] "+format, args...)
}

// ensure grows b to at least n float32 slots.
func ensure(b []float32, n int) []float32 {
	if cap(b) >= n {
		return b[:n]
	}
	return make([]float32, n)
}

// floatView maps a host byte buffer onto float32 when aligned and large enough;
// otherwise the caller stages through Go memory.
func floatView(b []byte, n int) ([]float32, bool) {
	if n <= 0 || len(b) < n*4 {
		return nil, false
	}
	if uintptr(unsafe.Pointer(&b[0]))%4 != 0 {
		return nil, false
	}
	return pcm.BytesF32(b[:n*4]), true
}
