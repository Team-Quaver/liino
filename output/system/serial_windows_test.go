//go:build windows

package system

import (
	"syscall"
	"testing"

	com "github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/com"
)

var procGetCurrentThreadID = syscall.NewLazyDLL("kernel32.dll").NewProc("GetCurrentThreadId")

func currentThreadID() uint32 {
	r, _, _ := procGetCurrentThreadID.Call()
	return uint32(r)
}

// TestSerialUsesOneMTAThread guards the contract behind serial: every
// audio-go COM call must run on the same dedicated MTA thread, no matter which
// host goroutine asks for it. If serial ever ran inline (or on a thread the
// host had put in the STA), WASAPI objects would be created in the wrong
// apartment and could silently stop producing audio on Windows.
func TestSerialUsesOneMTAThread(t *testing.T) {
	var (
		first, second uint32
		apt           com.APTTYPE
		qual          com.APTTYPEQUALIFIER
		aptErr        error
	)
	if err := serial(func() {
		first = currentThreadID()
		aptErr = com.CoGetApartmentType(&apt, &qual)
	}); err != nil {
		t.Fatalf("serial: %v", err)
	}
	if aptErr != nil {
		t.Fatalf("CoGetApartmentType: %v", aptErr)
	}
	if err := serial(func() { second = currentThreadID() }); err != nil {
		t.Fatalf("second serial: %v", err)
	}
	if first != second {
		t.Fatalf("serial ran on two threads (%d then %d); COM objects would cross apartments", first, second)
	}
	if apt != com.APTTYPE_MTA {
		t.Fatalf("serial thread apartment = %v, want MTA", apt)
	}
}
