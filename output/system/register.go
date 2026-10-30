//go:build windows || darwin

package system

import "github.com/team-quaver/liino/output"

// RegisterNamed adds an audio-go backed driver to the output registry.
func RegisterNamed(d output.Driver) { output.Register(d) }
