//go:build windows && (amd64 || arm64)

package wmf

import (
	"fmt"
	"runtime"
	"sync"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/media/mediafoundation"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/com"
)

// mta owns the one OS thread that talks to Media Foundation.
//
// Media Foundation is a multithreaded-apartment API. Hosts routinely
// initialize their UI thread as STA: MyGo (Quaver Astra) does exactly that in
// its Windows backend, and Electron's browser process does the same before any
// JavaScript runs. A Go goroutine can be scheduled onto that OS thread at any
// moment, so a per-call CoInitializeEx(MTA) fails with RPC_E_CHANGED_MODE
// (0x80010106) on the host's UI thread - and MF objects created in the MTA are
// not meant to be driven from an STA thread in the first place.
//
// The old implementation did the per-call init and surfaced that failure as
// decode.ErrUnavailable, which the engine classifies as fatal ("backend
// unavailable"), so Windows playback never started. Instead, every Media
// Foundation call - MFStartup included - now runs on this dedicated,
// MTA-locked thread. The backend no longer depends on the apartment the host
// happens to use, and the source reader is created and used from one and the
// same thread.
type mta struct {
	once    sync.Once
	ready   chan struct{}
	jobs    chan func()
	initErr error
}

var worker mta

// start initializes the worker (once) and reports why it could not start.
func (m *mta) start() error {
	m.once.Do(func() {
		m.ready = make(chan struct{})
		m.jobs = make(chan func(), 16)
		go m.run()
	})
	<-m.ready
	return m.initErr
}

func (m *mta) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hr, err := com.CoInitializeEx(uint32(com.COINIT_MULTITHREADED))
	if err != nil {
		m.initErr = fmt.Errorf("CoInitializeEx(MTA): %w (hr=0x%08x)", err, uint32(hr))
		close(m.ready)
		return
	}
	defer com.CoUninitialize()

	m.initErr = mediafoundation.MFStartup(mediafoundation.MF_VERSION, mediafoundation.MFSTARTUP_FULL)
	close(m.ready)
	if m.initErr != nil {
		return
	}
	for job := range m.jobs {
		job()
	}
}

// do runs fn on the MTA thread and waits for it to finish. fn must not call
// do again (the worker would deadlock waiting on itself).
func (m *mta) do(fn func()) error {
	if err := m.start(); err != nil {
		return err
	}
	done := make(chan struct{})
	var panicked any
	m.jobs <- func() {
		defer close(done)
		defer func() { panicked = recover() }()
		fn()
	}
	<-done
	if panicked != nil {
		panic(panicked)
	}
	return nil
}
