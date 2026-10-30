package output

import (
	"errors"
	"sync"
	"time"

	"github.com/team-quaver/liino/pcm"
)

// Null is the fallback driver: a timer-driven fake device that consumes audio
// at the nominal rate. Tests, headless CI and "no device" machines use it, and
// it lets the engine exercise the whole pipeline without touching the OS.
type nullDriver struct{}

func (nullDriver) Name() string  { return "null" }
func (nullDriver) Priority() int { return 100 }
func (nullDriver) Probe() (bool, string) {
	return true, "timer-driven sink"
}

func (nullDriver) Devices() ([]Device, error) {
	return []Device{{ID: "null", Driver: "null", Name: "Null device", Default: true,
		Formats: []pcm.Format{pcm.DefaultFormat()}}}, nil
}

func (nullDriver) Open(opts Options) (Output, error) {
	if opts.Source == nil {
		return nil, errors.New("output null: source required")
	}
	opts.withDefaults()
	n := &nullOutput{
		format: opts.Format,
		period: opts.PeriodFrames,
		pump:   NewPump(opts.Source, opts.Format, opts.OnUnderrun, opts.OnError, opts.Log),
		device: Device{ID: "null", Driver: "null", Name: "Null device", Default: true},
		done:   make(chan struct{}),
	}
	return n, nil
}

type nullOutput struct {
	format pcm.Format
	period int
	pump   *Pump
	device Device

	mu      sync.Mutex
	state   State
	running bool
	done    chan struct{}
	wg      sync.WaitGroup
}

func (n *nullOutput) Start() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.state == StateClosed {
		return ErrClosed
	}
	if n.running {
		return nil
	}
	n.running = true
	n.state = StateRunning
	n.done = make(chan struct{})
	n.wg.Add(1)
	tick := time.NewTicker(time.Duration(n.period) * time.Second / time.Duration(n.format.SampleRate))
	go func() {
		defer n.wg.Done()
		defer tick.Stop()
		buf := make([]float32, n.period*n.format.Channels)
		for {
			select {
			case <-n.done:
				return
			case <-tick.C:
				n.mu.Lock()
				consuming := n.running && n.state == StateRunning
				n.mu.Unlock()
				if !consuming {
					continue
				}
				n.pump.Fill(buf, n.period)
			}
		}
	}()
	return nil
}

func (n *nullOutput) Pause() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.state == StateClosed {
		return ErrClosed
	}
	n.state = StatePaused
	return nil
}

func (n *nullOutput) Resume() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.state == StateClosed {
		return ErrClosed
	}
	n.state = StateRunning
	return nil
}

func (n *nullOutput) Stop() error {
	n.mu.Lock()
	if n.state == StateClosed {
		n.mu.Unlock()
		return ErrClosed
	}
	wasRunning := n.running
	n.running = false
	n.state = StateStopped
	if wasRunning {
		close(n.done)
	}
	n.mu.Unlock()
	if wasRunning {
		n.wg.Wait()
	}
	return nil
}

func (n *nullOutput) Close() error {
	if err := n.Stop(); err != nil && !errors.Is(err, ErrClosed) {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.state = StateClosed
	return nil
}

func (n *nullOutput) State() State {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.state
}

func (n *nullOutput) Device() Device     { return n.device }
func (n *nullOutput) Format() pcm.Format { return n.format }
func (n *nullOutput) Latency() time.Duration {
	return time.Duration(n.period) * time.Second / time.Duration(n.format.SampleRate)
}

func (n *nullOutput) FramesWritten() uint64 {
	w, _ := n.pump.Counters()
	return w
}

func (n *nullOutput) Underruns() uint64 {
	_, u := n.pump.Counters()
	return u
}

func init() { Register(nullDriver{}) }
