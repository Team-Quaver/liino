//go:build windows || darwin

package system

import (
	"testing"
	"time"

	"github.com/team-quaver/liino/output"
	"github.com/team-quaver/liino/pcm"
)

// silenceSource is a Source that always supplies full frames.
type silenceSource struct{}

func (silenceSource) Read(dst []float32) int {
	clear(dst)
	return len(dst) / pcm.DefaultChannels
}

func (silenceSource) Reset() {}

// TestDataCallbackNeverTakesSessionLock guards the realtime contract of data.
//
// WASAPI and Core Audio prefill their first device buffers synchronously from
// inside start(), which means the data callback runs on the goroutine that
// holds session.mu in Start. The callback used to call State() (which locks
// mu), deadlocking before the device ever played a frame. This test reproduces
// that exact lock state: if data ever takes mu again, it never returns and the
// test fails.
func TestDataCallbackNeverTakesSessionLock(t *testing.T) {
	format := pcm.Format{SampleRate: pcm.DefaultSampleRate, Channels: pcm.DefaultChannels}
	s := &session{
		format: format,
		pump:   output.NewPump(silenceSource{}, format, nil, nil, nil),
	}
	s.state = output.StateRunning
	s.running.Store(true)

	s.mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		out := make([]byte, 480*pcm.DefaultChannels*4)
		s.data(out, nil, 480)
	}()
	select {
	case <-done:
		s.mu.Unlock()
	case <-time.After(2 * time.Second):
		s.mu.Unlock()
		t.Fatal("data callback blocked on session.mu; realtime callbacks must only read atomics")
	}
}
