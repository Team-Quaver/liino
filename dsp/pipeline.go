package dsp

import (
	"errors"
	"sync"
)

// Stage describes one entry of the pipeline for diagnostics.
type Stage struct {
	Kind    string `json:"kind"` // "eq", "chain", "gain", "limiter", "fade", "meter"
	Enabled bool   `json:"enabled"`
	Detail  string `json:"detail,omitempty"`
}

// State is a JSON-friendly snapshot of the pipeline.
type State struct {
	SampleRate int      `json:"sample_rate"`
	Channels   int      `json:"channels"`
	Bypass     bool     `json:"bypass"`
	PreampDB   float64  `json:"preamp_db"`
	Bands      []Config `json:"bands"`
	Limiter    bool     `json:"limiter"`
	ChainLen   int      `json:"chain_len"`
	Fade       string   `json:"fade,omitempty"`
	FadeDone   bool     `json:"fade_done"`
	Stages     []Stage  `json:"stages"`
}

// Pipeline is the ordered DSP stage set the engine runs between decode and
// output: EQ bands, the user effects chain, preamp/volume, limiter, fades, and
// a read-only meter.
//
// The whole thing is a Processor, so the engine treats it like any other stage.
// It is called from the decode goroutine, never from the device callback, so
// parameter changes are mutex-guarded and lock-free for the audio path.
type Pipeline struct {
	mu       sync.Mutex
	rate     float64
	channels int

	bands     []*band
	chain     []Processor
	preamp    *RampGain
	limiter   *Limiter
	limiterOn bool
	fade      *Fade
	meter     *Meter
	bypass    bool
}

type band struct {
	cfg Config
	bq  *Biquad
}

// NewPipeline builds an empty pipeline at the canonical rate.
func NewPipeline(rate, channels int) *Pipeline {
	if rate <= 0 {
		rate = 48000
	}
	if channels <= 0 {
		channels = 2
	}
	return &Pipeline{
		rate:     float64(rate),
		channels: channels,
		preamp:   NewRampGain(float64(rate)),
		meter:    &Meter{},
	}
}

// SetFormat re-derives the filters after a rate or channel change.
func (p *Pipeline) SetFormat(rate, channels int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rate > 0 {
		// Rebuilding the gain stage must keep the user's target: SetFormat runs
		// asynchronously when a stream opens, after the host has already applied
		// the volume slider. Resetting to unity here silently made every track
		// start at 100% and ignored the saved volume until the next move.
		gain := 1.0
		if p.preamp != nil {
			gain = p.preamp.Linear()
		}
		p.rate = float64(rate)
		p.preamp = NewRampGain(p.rate)
		p.preamp.SetLinear(gain)
		for _, b := range p.bands {
			b.bq.SetRate(p.rate)
		}
		if p.limiter != nil {
			p.limiter.Reset()
		}
	}
	if channels > 0 {
		p.channels = channels
	}
}

// SetPreampDB moves the master gain toward a dB target with a ramp.
func (p *Pipeline) SetPreampDB(db float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.preamp.SetDB(db)
}

// SetVolume sets the master gain linearly (0..1 plus headroom above 1).
func (p *Pipeline) SetVolume(v float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.preamp.SetLinear(v)
}

// Volume reports the master gain target.
func (p *Pipeline) Volume() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.preamp.Linear()
}

// SetBands installs the EQ. Existing filters keep their internal state when the
// count and kinds match, so dragging a band while playing does not click.
func (p *Pipeline) SetBands(configs []Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := make([]*band, 0, len(configs))
	for i, cfg := range configs {
		if cfg.Freq <= 0 {
			return errors.New("dsp: band frequency must be positive")
		}
		if cfg.Q <= 0 {
			cfg.Q = 1
		}
		if i < len(p.bands) && p.bands[i].cfg.Shelf == cfg.Shelf {
			reuse := p.bands[i]
			reuse.bq.Update(cfg.Freq, cfg.Q, cfg.Gain)
			reuse.cfg = cfg
			next = append(next, reuse)
			continue
		}
		bq, err := cfg.NewBiquad(p.rate)
		if err != nil {
			return err
		}
		next = append(next, &band{cfg: cfg, bq: bq})
	}
	p.bands = next
	return nil
}

// Bands reports the current EQ configuration.
func (p *Pipeline) Bands() []Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Config, 0, len(p.bands))
	for _, b := range p.bands {
		out = append(out, b.cfg)
	}
	return out
}

// SetLimiter enables or re-tunes the safety limiter.
func (p *Pipeline) SetLimiter(enabled bool, thresholdDB, attackMs, releaseMs float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.limiterOn = enabled
	if !enabled {
		p.limiter = nil
		return
	}
	if thresholdDB == 0 && p.limiter != nil && p.limiter.rate == p.rate {
		return
	}
	p.limiter = NewLimiter(thresholdDB, attackMs, releaseMs, p.rate)
}

// SetChain installs user effects that run after the EQ. Passing nil clears it.
func (p *Pipeline) SetChain(chain []Processor) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chain = append([]Processor(nil), chain...)
}

// AppendChain adds one processor to the user effects chain.
func (p *Pipeline) AppendChain(proc Processor) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chain = append(p.chain, proc)
}

// SetBypass is a true bypass: nothing runs and all state is dropped, so
// re-enabling never replays a stale filter tail.
func (p *Pipeline) SetBypass(bypass bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bypass = bypass
	if bypass {
		p.resetLocked()
	}
}

// Bypass reports whether the pipeline is transparent.
func (p *Pipeline) Bypass() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bypass
}

// StartFade begins a fade from the current level.
func (p *Pipeline) StartFade(kind FadeKind, ms float64, curve FadeCurve) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if kind == FadeNone || ms <= 0 {
		p.fade = nil
		return
	}
	p.fade = NewFade(kind, ms, p.rate, curve)
}

// FadeDone reports whether the running fade finished.
func (p *Pipeline) FadeDone() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fade == nil || p.fade.Done()
}

// Level drains the meter window.
func (p *Pipeline) Level() Level {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.meter.Read()
}

// Process runs the whole chain in place.
func (p *Pipeline) Process(data []float32, channels int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bypass {
		return
	}
	if channels <= 0 {
		channels = p.channels
	}
	for _, b := range p.bands {
		b.bq.Process(data, channels)
	}
	for _, proc := range p.chain {
		if proc != nil {
			proc.Process(data, channels)
		}
	}
	p.preamp.Process(data, channels)
	if p.limiterOn && p.limiter != nil {
		p.limiter.Process(data, channels)
	}
	if p.fade != nil {
		p.fade.Process(data, channels)
		if p.fade.Done() && p.fade.kind == FadeOut {
			p.fade = nil
		}
	}
	p.meter.Process(data, channels)
}

// Reset drops filter memory, e.g. on seek or track change.
func (p *Pipeline) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resetLocked()
}

func (p *Pipeline) resetLocked() {
	for _, b := range p.bands {
		b.bq.Reset()
	}
	for _, proc := range p.chain {
		if proc != nil {
			proc.Reset()
		}
	}
	p.preamp.Reset()
	if p.limiter != nil {
		p.limiter.Reset()
	}
	p.fade = nil
}

// State snapshots the pipeline for the UI and the doctor command.
func (p *Pipeline) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := State{
		SampleRate: int(p.rate),
		Channels:   p.channels,
		Bypass:     p.bypass,
		PreampDB:   p.preamp.DB(),
		Bands:      make([]Config, 0, len(p.bands)),
		Limiter:    p.limiterOn,
		ChainLen:   len(p.chain),
		FadeDone:   p.fade == nil || p.fade.Done(),
	}
	for _, b := range p.bands {
		st.Bands = append(st.Bands, b.cfg)
	}
	if p.fade != nil {
		switch p.fade.kind {
		case FadeIn:
			st.Fade = "in"
		case FadeOut:
			st.Fade = "out"
		}
	}
	st.Stages = append(st.Stages,
		Stage{Kind: "eq", Enabled: len(st.Bands) > 0, Detail: itoa(len(st.Bands)) + " bands"},
		Stage{Kind: "chain", Enabled: st.ChainLen > 0, Detail: itoa(st.ChainLen) + " processors"},
		Stage{Kind: "gain", Enabled: true, Detail: ftoa(st.PreampDB) + " dB"},
		Stage{Kind: "limiter", Enabled: st.Limiter},
		Stage{Kind: "fade", Enabled: st.Fade != ""},
		Stage{Kind: "meter", Enabled: true},
	)
	return st
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func ftoa(v float64) string {
	return trimFloat(v, 2)
}

// LevelPeek reads the meter window without clearing it.
func (p *Pipeline) LevelPeek() Level {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.meter.Peek()
}
