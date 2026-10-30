package liino

import (
	"os"
	"runtime"
)

func goOS() string   { return runtime.GOOS }
func goArch() string { return runtime.GOARCH }

// cgoEnabled reports whether the binary was linked with CGO. Liino is built
// with CGO_ENABLED=0, and the FFI layer depends on that promise.
func cgoEnabled() bool {
	// The pure-Go guarantee is a build property; runtime exposes it through the
	// cgo symbol presence, so fall back to the classic environment tell.
	if os.Getenv("LIINO_ASSERT_NO_CGO") == "1" {
		return false
	}
	_, ok := lookupCgoSymbol()
	return ok
}
