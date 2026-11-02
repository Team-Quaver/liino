//go:build darwin

package system

// serial calls fn directly on macOS. Core Audio has no COM apartments, so the
// caller's thread is fine; the helper only exists so the shared driver code can
// stay identical across platforms.
func serial(fn func()) error {
	fn()
	return nil
}
