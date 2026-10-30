//go:build darwin

package system

import (
	audiogo "github.com/vcaesar/audio-go"
)

// Core Audio covers the default output device plus explicit device selection.
func init() {
	RegisterNamed(&driver{name: "coreaudio", kind: audiogo.BackendCoreaudio, priority: 0})
}
