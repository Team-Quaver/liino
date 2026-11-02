//go:build windows

package system

import (
	"fmt"
	"runtime"
	"sync"
	"time"

	com "github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/com"
)

// rpcEChangedMode is COM's RPC_E_CHANGED_MODE: CoInitializeEx asked for a
// different apartment model than the one the thread already joined.
const rpcEChangedMode = 0x80010106

// comInitAttempts bounds the number of threads tried before COM init is
// treated as failed; see comWorker.run.
const comInitAttempts = 4

// comWorker owns the one OS thread that talks to WASAPI.
//
// audio-go's COM calls reach ole32 directly. Whichever thread makes them joins
// an apartment: the Go scheduler can run a Liino goroutine on the host's UI
// thread (Electron's browser thread, MyGo's Windows backend both initialize it
// as STA), and COM objects created there belong to that STA. The render thread
// then drives those objects from a different thread, which is undefined for
// apartment-bound interfaces. It also means a host that initialized its UI
// thread as STA makes CoInitializeEx(MTA) fail with RPC_E_CHANGED_MODE.
//
// Keeping every device enumeration, open/start/stop and shutdown on one
// dedicated MTA thread removes the dependency on the apartment the host
// happens to use. The audio callback itself still runs on audio-go's own
// render thread, and only reads atomics (see session.data).
type comWorker struct {
	once    sync.Once
	ready   chan struct{}
	jobs    chan func()
	initErr error
}

var comThread comWorker

// start initializes the worker once and reports why it could not start.
func (w *comWorker) start() error {
	w.once.Do(func() {
		w.ready = make(chan struct{})
		w.jobs = make(chan func(), 16)
		go w.run(1)
	})
	<-w.ready
	return w.initErr
}

// run initializes one candidate OS thread and serves jobs once it is in the
// MTA. A goroutine may start life on a thread the host already put in the STA
// (the Electron browser thread when an FFI call enters Go, for instance).
// CoInitializeEx then reports RPC_E_CHANGED_MODE. That is recoverable: hand the
// thread back to the Go pool and retry from a fresh goroutine, because only
// host-created threads are COM-initialized.
func (w *comWorker) run(attempt int) {
	runtime.LockOSThread()
	locked := true
	defer func() {
		if locked {
			runtime.UnlockOSThread()
		}
	}()

	hr, err := com.CoInitializeEx(uint32(com.COINIT_MULTITHREADED))
	if err != nil {
		if uint32(hr) == rpcEChangedMode && attempt < comInitAttempts {
			// Do not let a locked goroutine exit: the runtime terminates the
			// OS thread, and it may be the host's UI thread. Unlock it first,
			// then retry after a pause so the scheduler is unlikely to reuse
			// the same thread.
			locked = false
			runtime.UnlockOSThread()
			go func() {
				time.Sleep(time.Millisecond)
				w.run(attempt + 1)
			}()
			return
		}
		w.initErr = fmt.Errorf("CoInitializeEx(MTA): %w (hr=0x%08x, attempt %d)", err, uint32(hr), attempt)
		close(w.ready)
		return
	}
	defer com.CoUninitialize()

	close(w.ready)
	for job := range w.jobs {
		job()
	}
}

// serial runs fn on the COM worker and waits for it to finish. fn must not call
// serial again (the worker would deadlock waiting on itself); code already
// running on the worker calls audio-go directly.
func serial(fn func()) error {
	if err := comThread.start(); err != nil {
		return err
	}
	done := make(chan struct{})
	var panicked any
	comThread.jobs <- func() {
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
