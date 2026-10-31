package liino

import (
	"encoding/json"
	"sync"
	"time"
)

// Kind classifies a playback event.
type Kind int

// Event kinds. The set mirrors what Quaver's transport and Astra's engine
// already consume, so the adapters stay thin.
const (
	// KindLoaded means a source opened and its metadata is known.
	KindLoaded Kind = iota
	// KindPlaying means audio is reaching the device.
	KindPlaying
	// KindPaused means the session is alive but muted by request.
	KindPaused
	// KindBuffering means the pipeline ran dry and is refilling.
	KindBuffering
	// KindReady means buffering finished and playback resumed.
	KindReady
	// KindSeeked means a seek took effect.
	KindSeeked
	// KindEnded means the stream hit end of file and drained.
	KindEnded
	// KindFailed means playback stopped with an error.
	KindFailed
	// KindDeviceLost means the output endpoint disappeared.
	KindDeviceLost
	// KindDeviceChanged means the active endpoint changed.
	KindDeviceChanged
	// KindClosed means the player released everything.
	KindClosed
)

func (k Kind) String() string {
	switch k {
	case KindLoaded:
		return "loaded"
	case KindPlaying:
		return "playing"
	case KindPaused:
		return "paused"
	case KindBuffering:
		return "buffering"
	case KindReady:
		return "ready"
	case KindSeeked:
		return "seeked"
	case KindEnded:
		return "ended"
	case KindFailed:
		return "failed"
	case KindDeviceLost:
		return "device-lost"
	case KindDeviceChanged:
		return "device-changed"
	case KindClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// Event is one notification about the player.
//
// Every event carries the load generation it belongs to. Consumers must drop
// events whose Gen is not the current one: that is the whole defence against
// late callbacks from a previous track re-appearing in the UI.
type Event struct {
	Kind     Kind      `json:"kind"`
	Name     string    `json:"name"`
	Gen      uint64    `json:"gen"` // load generation
	SeekGen  uint64    `json:"seek_gen"`
	At       time.Time `json:"at"`
	Snapshot Snapshot  `json:"snapshot"`
	Err      string    `json:"error,omitempty"`
}

// Snapshot is the player state at the time of the event or query.
type Snapshot struct {
	State      State  `json:"state"`
	Generation uint64 `json:"generation"`

	Position time.Duration `json:"position"`
	Duration time.Duration `json:"duration"`
	Buffered time.Duration `json:"buffered"`
	Latency  time.Duration `json:"latency"`

	Volume float64 `json:"volume"`
	Muted  bool    `json:"muted"`

	Driver     string `json:"driver"`
	Device     string `json:"device"`
	Decoder    string `json:"decoder"`
	Codec      string `json:"codec"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	Live       bool   `json:"live"`

	FramesWritten uint64 `json:"frames_written"`
	Underruns     uint64 `json:"underruns"`
	Drops         uint64 `json:"drops"`

	PeakL float64 `json:"peak_l_db"`
	PeakR float64 `json:"peak_r_db"`
	Clips uint64  `json:"clips"`

	Error string `json:"error,omitempty"`
}

// wireEvent is the FFI form of an Event: string kind, RFC3339 timestamp and a
// snapshot whose clock is in milliseconds. Go enums and nanosecond durations
// have no useful meaning across the C boundary, so they never cross it.
type wireEvent struct {
	Kind     string    `json:"kind"`
	Name     string    `json:"name"`
	Gen      uint64    `json:"gen"`
	SeekGen  uint64    `json:"seek_gen"`
	At       string    `json:"at"`
	Snapshot JSONShape `json:"snapshot"`
	Err      string    `json:"error,omitempty"`
}

// MarshalJSON implements json.Marshaler for events.
func (e Event) MarshalJSON() ([]byte, error) {
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	return json.Marshal(wireEvent{
		Kind:     e.Kind.String(),
		Name:     e.Name,
		Gen:      e.Gen,
		SeekGen:  e.SeekGen,
		At:       at.UTC().Format(time.RFC3339Nano),
		Snapshot: e.Snapshot.JSON(),
		Err:      e.Err,
	})
}

// JSONShape is the FFI-friendly form of a snapshot: whole numbers in
// milliseconds, so the Node side never parses Go durations.
type JSONShape struct {
	State      string  `json:"state"`
	Gen        uint64  `json:"gen"`
	PositionMS int64   `json:"position_ms"`
	DurationMS int64   `json:"duration_ms"`
	BufferedMS int64   `json:"buffered_ms"`
	LatencyMS  int64   `json:"latency_ms"`
	LatencyNS  int64   `json:"latency_ns"`
	Volume     float64 `json:"volume"`
	Muted      bool    `json:"muted"`
	Driver     string  `json:"driver"`
	Device     string  `json:"device"`
	Decoder    string  `json:"decoder"`
	Codec      string  `json:"codec"`
	SampleRate int     `json:"sample_rate"`
	Channels   int     `json:"channels"`
	Live       bool    `json:"live"`
	Underruns  uint64  `json:"underruns"`
	Clips      uint64  `json:"clips"`
	PeakL      float64 `json:"peak_l_db"`
	PeakR      float64 `json:"peak_r_db"`
	Error      string  `json:"error,omitempty"`
}

// JSON renders the snapshot for the FFI boundary.
func (s Snapshot) JSON() JSONShape {
	return JSONShape{
		State:      s.State.String(),
		Gen:        s.Generation,
		PositionMS: ms(s.Position),
		DurationMS: ms(s.Duration),
		BufferedMS: ms(s.Buffered),
		LatencyMS:  ms(s.Latency),
		LatencyNS:  int64(s.Latency),
		Volume:     s.Volume,
		Muted:      s.Muted,
		Driver:     s.Driver,
		Device:     s.Device,
		Decoder:    s.Decoder,
		Codec:      s.Codec,
		SampleRate: s.SampleRate,
		Channels:   s.Channels,
		Live:       s.Live,
		Underruns:  s.Underruns,
		Clips:      s.Clips,
		PeakL:      s.PeakL,
		PeakR:      s.PeakR,
		Error:      s.Error,
	}
}

func ms(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return int64(d / time.Millisecond)
}

// eventBus fans events out to subscribers without ever blocking the caller.
type eventBus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
	seq  uint64
}

func newEventBus() *eventBus { return &eventBus{subs: map[chan Event]struct{}{}} }

// subscribe registers a listener with its own buffered channel.
func (b *eventBus) subscribe(size int) chan Event {
	if size <= 0 {
		size = 64
	}
	ch := make(chan Event, size)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// unsubscribe closes and removes a listener.
func (b *eventBus) unsubscribe(ch chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
}

// publish delivers a copy to every subscriber, dropping the oldest event when
// a subscriber falls behind instead of blocking the pipeline.
func (b *eventBus) publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	ev.At = time.Now()
	for ch := range b.subs {
		for {
			select {
			case ch <- ev:
				goto next
			default:
				// Full: drop the oldest and retry once in a while.
				select {
				case <-ch:
				default:
				}
				select {
				case ch <- ev:
					goto next
				default:
					goto next
				}
			}
		}
	next:
	}
}

func (b *eventBus) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
