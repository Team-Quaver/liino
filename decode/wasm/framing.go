package wasm

import (
	"encoding/binary"
	"errors"
	"io"
)

// Frame kinds emitted by the helper.
const (
	kindJSON byte = 'J'
	kindPCM  byte = 'P'
)

// maxPayload caps one frame at 8 MB, far above any sane audio packet.
const maxPayload = 8 << 20

// framer reads [uint32 BE length][1 byte kind][payload] frames.
type framer struct {
	r    io.Reader
	head [5]byte
	buf  []byte
}

func newFramer(r io.Reader) *framer {
	return &framer{r: r, buf: make([]byte, 0, 1<<16)}
}

func (f *framer) read() ([]byte, byte, error) {
	if _, err := io.ReadFull(f.r, f.head[:]); err != nil {
		return nil, 0, err
	}
	length := binary.BigEndian.Uint32(f.head[:4])
	kind := f.head[4]
	if length > maxPayload {
		return nil, 0, errors.New("decode wasm: frame too large")
	}
	if cap(f.buf) < int(length) {
		f.buf = make([]byte, length)
	}
	f.buf = f.buf[:length]
	if _, err := io.ReadFull(f.r, f.buf); err != nil {
		return nil, 0, err
	}
	return f.buf, kind, nil
}
