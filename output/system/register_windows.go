//go:build windows

package system

import (
	audiogo "github.com/vcaesar/audio-go"
)

// WASAPI is the only Windows driver Liino uses: shared mode, in-process,
// AUTOCONVERTPCM handled by audio-go.
func init() {
	RegisterNamed(&driver{name: "wasapi", kind: audiogo.BackendWasapi, priority: 0})
}
