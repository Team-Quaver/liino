//go:build cgo

package liino

func lookupCgoSymbol() (struct{}, bool) { return struct{}{}, true }
