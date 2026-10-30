package liino

import "errors"

// Errors returned by the Player API.
var (
	// ErrClosed is returned once the player has been closed.
	ErrClosed = errors.New("liino: player closed")
	// ErrNoMedia is returned when playing without a loaded source.
	ErrNoMedia = errors.New("liino: no media loaded")
	// ErrNotRunning is returned for operations that need an active session.
	ErrNotRunning = errors.New("liino: not running")
	// ErrSeekUnsupported is returned for live or non-seekable sources.
	ErrSeekUnsupported = errors.New("liino: seek not supported")
	// ErrInvalidConfig marks unusable configuration.
	ErrInvalidConfig = errors.New("liino: invalid config")
)
