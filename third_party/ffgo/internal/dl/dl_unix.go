//go:build !windows

package dl

import "github.com/bnema/purego"

// Open loads a shared library. RTLD_GLOBAL is required: FFmpeg libraries
// reference symbols across library boundaries (libavformat needs libavcodec,
// which needs libavutil).
func Open(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}

// Close unloads a library. FFmpeg handles stay alive for the process lifetime,
// so this exists only for symmetry with the loader API.
func Close(handle uintptr) error {
	if handle == 0 {
		return nil
	}
	return purego.Dlclose(handle)
}
