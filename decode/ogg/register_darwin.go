//go:build darwin

package ogg

import "github.com/team-quaver/liino/decode"

func init() { decode.Register(backend{}) }
