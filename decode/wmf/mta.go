//go:build windows && (amd64 || arm64)

package wmf

import (
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/media/mediafoundation"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/com"
)

// rpcEChangedMode is COM's RPC_E_CHANGED_MODE: CoInitializeEx was called with
// a different apartment model than the one the thread already joined.
const rpcEChangedMode = 0x80010106

// mtaInitAttempts bounds how many threads the worker will try before giving up.
// Only host-created threads can be in the STA, and the Go runtime creates every
// other thread, so a retry almost always lands on a clean one; the cap keeps a
// pathological host from spinning forever.
const mtaInitAttempts = 4

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
		go m.run(1)
	})
	<-m.ready
	return m.initErr
}

// run initializes one candidate OS thread and, once COM accepts the MTA,
// serves jobs forever. attempt counts the threads tried so far.
//
// A goroutine can start life on an OS thread the host already initialized as
// STA (the MyGo UI thread, or the Electron browser thread when an FFI call
// brings control into Go). CoInitializeEx then fails with RPC_E_CHANGED_MODE.
// That is not a dead end: unlock the thread, return it to the Go pool, and try
// again from a fresh goroutine. All non-host threads are uninitialized, so a
// bounded number of retries is enough even on a single-P machine.
func (m *mta) run(attempt int) {
	runtime.LockOSThread()
	locked := true
	defer func() {
		if locked {
			runtime.UnlockOSThread()
		}
	}()

	hr, err := com.CoInitializeEx(uint32(com.COINIT_MULTITHREADED))
	if err != nil {
		if uint32(hr) == rpcEChangedMode && attempt < mtaInitAttempts {
			// Do not let a locked goroutine exit: the runtime terminates the
			// thread, and this one may be the host's UI thread. Hand it back to
			// the pool, then let the replacement start after a short pause so
			// the scheduler is unlikely to put it back on the same thread.
			locked = false
			runtime.UnlockOSThread()
			go func() {
				time.Sleep(time.Millisecond)
				m.run(attempt + 1)
			}()
			return
		}
		m.initErr = fmt.Errorf("CoInitializeEx(MTA): %w (hr=0x%08x, attempt %d)", err, uint32(hr), attempt)
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
