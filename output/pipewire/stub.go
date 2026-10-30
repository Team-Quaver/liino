//go:build !linux

// Package pipewire is Linux-only: PipeWire is the audio server Liino targets
// there. On other platforms WASAPI (Windows) and Core Audio (macOS) register
// their own drivers.
package pipewire
