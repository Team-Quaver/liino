//go:build !windows && !darwin

// Package system only builds drivers on Windows (WASAPI) and macOS (Core
// Audio). On Linux the PipeWire driver is used instead and ALSA is deliberately
// not registered, so Liino never grabs /dev/snd away from the session server.
package system
