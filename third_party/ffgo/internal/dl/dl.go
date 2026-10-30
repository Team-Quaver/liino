// Package dl is the shared-library loader used by ffgo's bindings.
//
// Upstream ffgo calls purego.Dlopen directly, which only exists on the Unix
// ports of purego, so the fork cannot even compile on Windows. Everything the
// loader needs is a module handle: symbol resolution already works there,
// because purego.loadSymbol falls back to syscall.GetProcAddress on Windows.
//
// This package keeps one entry point, Open, with a Unix implementation on top
// of purego and a Windows implementation on top of LoadLibraryExW. It is the
// core of the local patch; see ../VENDORED.md.
package dl
