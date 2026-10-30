//go:build windows

package dl

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

var (
	modkernel32        = syscall.NewLazyDLL("kernel32.dll")
	procLoadLibraryExW = modkernel32.NewProc("LoadLibraryExW")
	procFreeLibrary    = modkernel32.NewProc("FreeLibrary")
)

// LOAD_WITH_ALTERED_SEARCH_PATH makes the DLL's own directory the first stop
// for its imports, which is what lets an app-local FFmpeg build (avcodec-XX.dll
// pulling in avutil-XX.dll) load without touching PATH.
const loadWithAlteredSearchPath = 0x00000008

// Open loads a DLL by full path or by name and returns its HMODULE.
func Open(path string) (uintptr, error) {
	if path == "" {
		return 0, fmt.Errorf("dl: empty library path")
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var flags uintptr
	if strings.ContainsAny(path, `/\`) {
		flags = loadWithAlteredSearchPath
	}
	r1, _, callErr := procLoadLibraryExW.Call(uintptr(unsafe.Pointer(name)), 0, flags)
	if r1 == 0 {
		return 0, fmt.Errorf("dl: LoadLibraryExW(%q): %w", path, callErr)
	}
	return r1, nil
}

// Close releases a module handle. Windows refcounts loads, so a library stays
// mapped until the last FreeLibrary.
func Close(handle uintptr) error {
	if handle == 0 {
		return nil
	}
	r1, _, callErr := procFreeLibrary.Call(handle)
	if r1 == 0 {
		return fmt.Errorf("dl: FreeLibrary: %w", callErr)
	}
	return nil
}
