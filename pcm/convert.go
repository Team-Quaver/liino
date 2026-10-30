package pcm

import (
	"unsafe"
)

// This file holds the shape conversions the pipeline needs at its edges:
// FFmpeg hands out planar or packed float buffers, PipeWire wants planar,
// WASAPI and CoreAudio want packed float32 byte buffers.

// Interleave folds planar channels into one interleaved buffer. Rows shorter
// than frames are zero padded, longer rows are ignored past the frame count.
func Interleave(dst []float32, planar [][]float32, frames int) int {
	ch := len(planar)
	if ch <= 0 || frames <= 0 {
		return 0
	}
	n := frames
	if n*ch > len(dst) {
		n = len(dst) / ch
	}
	for i := 0; i < n; i++ {
		for c := 0; c < ch; c++ {
			v := float32(0)
			if row := planar[c]; i < len(row) {
				v = row[i]
			}
			dst[i*ch+c] = v
		}
	}
	return n
}

// Deinterleave splits an interleaved buffer into planar rows. Rows are
// resized as needed; stride is the per-channel advance inside each row.
func Deinterleave(planar [][]float32, src []float32, frames, stride int) int {
	ch := len(planar)
	if ch <= 0 || frames <= 0 {
		return 0
	}
	if stride <= 0 {
		stride = 1
	}
	n := frames
	if n*ch > len(src) {
		n = len(src) / ch
	}
	for c := 0; c < ch; c++ {
		row := planar[c]
		if len(row) < n*stride {
			row = make([]float32, n*stride)
			planar[c] = row
		}
		for i := 0; i < n; i++ {
			// Clear the whole row so padded slots stay silent.
			for s := 0; s < stride; s++ {
				row[i*stride+s] = 0
			}
			row[i*stride] = src[i*ch+c]
		}
	}
	return n
}

// F32Bytes reinterprets a float32 slice as bytes for platform APIs that hand
// out []byte buffers. The slice must be Go-allocated so 4-byte alignment holds.
func F32Bytes(f []float32) []byte {
	if len(f) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&f[0])), len(f)*4)
}

// BytesF32 reinterprets an aligned byte buffer as interleaved float32. Use it
// only on buffers allocated by Go or otherwise guaranteed to be 4-byte aligned.
func BytesF32(b []byte) []float32 {
	n := len(b) / 4
	if n == 0 {
		return nil
	}
	return unsafe.Slice((*float32)(unsafe.Pointer(&b[0])), n)
}

// Scale multiplies every sample by g in place.
func Scale(data []float32, g float32) {
	if g == 1 {
		return
	}
	for i := range data {
		data[i] *= g
	}
}

// CopyFrames copies up to frames interleaved frames from src into dst and
// returns the frame count written. Unused slots in dst are zeroed so audio
// callbacks never emit stale samples.
func CopyFrames(dst, src []float32, frames, channels int) int {
	if channels <= 0 {
		channels = DefaultChannels
	}
	room := len(dst) / channels
	avail := len(src) / channels
	n := min(room, min(avail, frames))
	copy(dst[:n*channels], src[:n*channels])
	clear(dst[n*channels : room*channels])
	return n
}
