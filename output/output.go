// Package output is the playback edge of the Liino pipeline.
//
// Every driver here talks to the operating system's own audio API from inside
// the process - WASAPI on Windows, Core Audio on macOS, PipeWire on Linux -
// with no helper binary, no IPC and no CGO. Staying in-process is not a
// micro-optimisation: it is what keeps Windows per-app audio capture working,
// because the render session belongs to Quaver Music's own process, so Discord,
// OBS, Kook and the rest route exactly Liino's stream and nothing else.
//
// Drivers are pull-based. The system callback asks for N frames, the Pump hands
// over whatever the pipeline has buffered, and missing frames become silence.
package output

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/team-quaver/liino/pcm"
)

// Errors returned by the output layer.
var (
	// ErrNoDriver means no audio driver can run in this process.
	ErrNoDriver = errors.New("output: no audio driver available")
	// ErrUnavailable means a specific driver is missing on this platform.
	ErrUnavailable = errors.New("output: driver unavailable")
	// ErrNoSuchDevice means the requested device id is gone.
	ErrNoSuchDevice = errors.New("output: no such device")
	// ErrClosed is returned once an output has been closed.
	ErrClosed = errors.New("output: closed")
)

// DefaultPeriodFrames is 5 ms at 48 kHz, the granularity every driver accepts.
const DefaultPeriodFrames = 240

// Device is one selectable output endpoint.
type Device struct {
	ID      string       // stable, driver-scoped identifier
	Driver  string       // driver that reported it
	Name    string       // human readable label
	Default bool         // system default output
	Native  string       // OS-level identifier, for diagnostics
	Formats []pcm.Format // formats the endpoint advertises, when known
}

// String renders the device for menus and logs.
func (d Device) String() string {
	var b strings.Builder
	b.WriteString(d.Name)
	if d.Driver != "" {
		fmt.Fprintf(&b, " [%s]", d.Driver)
	}
	if d.Default {
		b.WriteString(" (default)")
	}
	return b.String()
}

// Source is the pipeline end an output pulls from. Implementations must never
// block: the callback runs on a system audio thread.
type Source interface {
	// Read copies at most len(dst)/Channels interleaved float32 frames into dst
	// and returns the frame count actually supplied.
	Read(dst []float32) int
	// Reset drops any per-session state, e.g. on a device switch.
	Reset()
}

// Options configures one output session.
type Options struct {
	DeviceID     string     // empty selects the default endpoint
	Format       pcm.Format // canonical pipeline format
	PeriodFrames int        // per-callback frame count, 0 uses the default
	AppName      string     // "Quaver Music" - shown by the OS mixer
	StreamName   string     // track title or "playback"
	Source       Source     // required

	// OnUnderrun reports frames the pipeline could not supply.
	OnUnderrun func(missing int)
	// OnError reports async failures (device lost, stream error).
	OnError func(err error)
	// Log receives driver diagnostics.
	Log func(string)
}

func (o *Options) withDefaults() {
	if o.Format.SampleRate <= 0 {
		o.Format.SampleRate = pcm.DefaultSampleRate
	}
	if o.Format.Channels <= 0 {
		o.Format.Channels = pcm.DefaultChannels
	}
	if o.PeriodFrames <= 0 {
		o.PeriodFrames = DefaultPeriodFrames
	}
	if o.AppName == "" {
		o.AppName = "Liino"
	}
	if o.StreamName == "" {
		o.StreamName = "playback"
	}
}

// State enumerates the lifecycle of a session.
type State int

// Lifecycle states.
const (
	StateIdle State = iota
	StateRunning
	StatePaused
	StateStopped
	StateClosed
)

func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateRunning:
		return "running"
	case StatePaused:
		return "paused"
	case StateStopped:
		return "stopped"
	case StateClosed:
		return "closed"
	default:
		return fmt.Sprintf("state(%d)", int(s))
	}
}

// Output is an opened audio session.
type Output interface {
	// Start begins or restarts the pull loop.
	Start() error
	// Pause keeps the device session but stops requesting audio.
	Pause() error
	// Resume undoes Pause.
	Resume() error
	// Stop tears down the stream while keeping the handle for a later Start.
	Stop() error
	// Close releases every resource.
	Close() error

	State() State
	Device() Device
	Format() pcm.Format
	// FramesWritten counts real audio frames handed to the device; silence
	// padding is excluded, so it is safe to use as the playback clock.
	FramesWritten() uint64
	// Underruns counts callback gaps.
	Underruns() uint64
	// Latency is the configured buffer depth.
	Latency() time.Duration
}

// Driver is one platform audio API implementation.
type Driver interface {
	Name() string
	Priority() int // lower wins when picking a default
	Probe() (ok bool, note string)
	Devices() ([]Device, error)
	Open(opts Options) (Output, error)
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Driver{}
)

// Register adds a driver. Platform drivers call it from init.
func Register(d Driver) {
	if d == nil {
		return
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[d.Name()] = d
}

// Unregister removes a driver (tests only).
func Unregister(name string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(registry, name)
}

// Lookup finds a driver by name.
func Lookup(name string) (Driver, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	d, ok := registry[name]
	return d, ok
}

// Drivers lists registered drivers by priority.
func Drivers() []Driver {
	registryMu.RLock()
	out := make([]Driver, 0, len(registry))
	for _, d := range registry {
		out = append(out, d)
	}
	registryMu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority() != out[j].Priority() {
			return out[i].Priority() < out[j].Priority()
		}
		return out[i].Name() < out[j].Name()
	})
	return out
}

// Default returns the best usable driver for this platform.
func Default() (Driver, error) {
	for _, d := range Drivers() {
		if ok, _ := d.Probe(); ok {
			return d, nil
		}
	}
	return nil, ErrNoDriver
}

// DeviceStatus describes a driver for diagnostics.
type DeviceStatus struct {
	Name      string `json:"name"`
	Priority  int    `json:"priority"`
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`
	Devices   int    `json:"devices"`
}

// Statuses reports every driver plus how many endpoints it sees.
func Statuses() []DeviceStatus {
	out := make([]DeviceStatus, 0, len(registry))
	for _, d := range Drivers() {
		ok, note := d.Probe()
		n := 0
		if ok {
			if devs, err := d.Devices(); err == nil {
				n = len(devs)
			}
		}
		out = append(out, DeviceStatus{Name: d.Name(), Priority: d.Priority(), Available: ok, Note: note, Devices: n})
	}
	return out
}

// AllDevices lists the endpoints of every usable driver.
func AllDevices() ([]Device, error) {
	var (
		out  []Device
		last error
	)
	for _, d := range Drivers() {
		if ok, _ := d.Probe(); !ok {
			continue
		}
		devs, err := d.Devices()
		if err != nil {
			last = err
			continue
		}
		out = append(out, devs...)
	}
	if len(out) == 0 && last != nil {
		return nil, last
	}
	return out, nil
}

// Open starts a session on the named driver, or the default one when name is
// empty.
func Open(name string, opts Options) (Output, Driver, error) {
	opts.withDefaults()
	if opts.Source == nil {
		return nil, nil, errors.New("output: Options.Source is required")
	}
	d, err := resolveDriver(name)
	if err != nil {
		return nil, nil, err
	}
	out, err := d.Open(opts)
	if err != nil {
		return nil, d, fmt.Errorf("output %s: %w", d.Name(), err)
	}
	return out, d, nil
}

func resolveDriver(name string) (Driver, error) {
	if name == "" {
		return Default()
	}
	d, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, name)
	}
	if ok, note := d.Probe(); !ok {
		if note != "" {
			return nil, fmt.Errorf("%w: %s", ErrUnavailable, note)
		}
		return nil, ErrUnavailable
	}
	return d, nil
}

// FindDevice resolves a device id across drivers, accepting either the opaque
// ID or "<driver>/<id>".
func FindDevice(id string) (Device, Driver, error) {
	if id == "" {
		d, err := Default()
		if err != nil {
			return Device{}, nil, err
		}
		devs, err := d.Devices()
		if err != nil {
			return Device{}, nil, err
		}
		return pickDefault(devs), d, nil
	}
	driverName, want, split := strings.Cut(id, "/")
	for _, d := range Drivers() {
		if ok, _ := d.Probe(); !ok {
			continue
		}
		if split && d.Name() != driverName {
			continue
		}
		devs, err := d.Devices()
		if err != nil {
			continue
		}
		for _, dev := range devs {
			if (split && dev.ID == want) || (!split && dev.ID == id) {
				return dev, d, nil
			}
		}
	}
	return Device{}, nil, fmt.Errorf("%w: %s", ErrNoSuchDevice, id)
}

func pickDefault(devs []Device) Device {
	var first Device
	for _, d := range devs {
		if first.Name == "" {
			first = d
		}
		if d.Default {
			return d
		}
	}
	return first
}

// Pump adapts a Source to the shapes the platform APIs want and keeps the
// counters every driver reports. It is the only place audio frames are copied
// on the callback side, and it never allocates after the first call.
type Pump struct {
	src    Source
	format pcm.Format

	scratch   []float32
	planar    [][]float32
	written   atomic.Uint64
	underruns atomic.Uint64

	onUnderrun func(int)
	onError    func(error)
	log        func(string)
}

// NewPump builds a pump for the given source and format.
func NewPump(src Source, format pcm.Format, onUnderrun func(int), onError func(error), logf func(string)) *Pump {
	if format.Channels <= 0 {
		format.Channels = pcm.DefaultChannels
	}
	if format.SampleRate <= 0 {
		format.SampleRate = pcm.DefaultSampleRate
	}
	return &Pump{src: src, format: format, onUnderrun: onUnderrun, onError: onError, log: logf}
}

// Fill writes frames interleaved float32 into dst (len(dst) == frames*Channels)
// and accounts for the shortfall as an underrun.
func (p *Pump) Fill(dst []float32, frames int) int {
	if frames <= 0 || len(dst) < frames*p.format.Channels {
		return 0
	}
	p.scratch = ensureFrames(p.scratch, frames)
	n := p.src.Read(p.scratch[:frames*p.format.Channels])
	if n > frames {
		n = frames
	}
	ch := p.format.Channels
	copy(dst[:n*ch], p.scratch[:n*ch])
	if gaps := frames - n; gaps > 0 {
		clear(dst[n*ch : frames*ch])
		p.underruns.Add(1)
		if p.onUnderrun != nil {
			p.onUnderrun(gaps)
		}
	}
	p.written.Add(uint64(n))
	return n
}

// FillPlanar writes frames into de-interleaved channel rows, for the APIs that
// want planar buffers (PipeWire).
func (p *Pump) FillPlanar(planar [][]float32, frames, stride int) int {
	if len(planar) == 0 || frames <= 0 {
		return 0
	}
	p.scratch = ensureFrames(p.scratch, frames)
	n := p.src.Read(p.scratch[:frames*p.format.Channels])
	if n > frames {
		n = frames
	}
	p.planar = ensurePlanar(p.planar, len(planar), frames*maxInt(stride, 1))
	pcm.Deinterleave(p.planar, p.scratch[:n*p.format.Channels], n, maxInt(stride, 1))
	for c := range planar {
		row := planar[c]
		lim := minInt(len(row), frames*maxInt(stride, 1))
		copy(row[:lim], p.planar[c][:lim])
	}
	if gaps := frames - n; gaps > 0 {
		p.underruns.Add(1)
		if p.onUnderrun != nil {
			p.onUnderrun(gaps)
		}
	}
	p.written.Add(uint64(n))
	return n
}

// Counters returns the frames handed to the device and the underrun count.
func (p *Pump) Counters() (written, underruns uint64) {
	return p.written.Load(), p.underruns.Load()
}

// Reset clears counters and source state, e.g. after a seek or a device switch.
func (p *Pump) Reset() {
	p.written.Store(0)
	p.underruns.Store(0)
	p.src.Reset()
}

// ReportError forwards an async driver failure to the owner.
func (p *Pump) ReportError(err error) {
	if p.onError != nil {
		p.onError(err)
	}
}

// Logf emits driver diagnostics when a sink was configured.
func (p *Pump) Logf(format string, args ...any) {
	if p.log != nil {
		p.log(fmt.Sprintf(format, args...))
	}
}

func ensureFrames(b []float32, frames int) []float32 {
	n := frames * pcm.DefaultChannels
	if cap(b) >= n {
		return b[:n]
	}
	return make([]float32, n)
}

func ensurePlanar(rows [][]float32, channels, length int) [][]float32 {
	if len(rows) != channels {
		rows = make([][]float32, channels)
	}
	for i := range rows {
		if len(rows[i]) < length {
			rows[i] = make([]float32, length)
		}
	}
	return rows
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
