//go:build windows && (amd64 || arm64)

package winrt

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/runtime/win32"
	rwinrt "github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/winrt"
)

// rpcEChangedMode is RPC_E_CHANGED_MODE: RoInitialize was called with an
// apartment model the thread already joined. The host (Electron's browser
// process, MyGo) initializes its UI thread as STA, and a Go goroutine can be
// scheduled onto that thread at any time, so the worker retries on a fresh
// thread when this happens - the same dance package wmf does for COM.
const rpcEChangedMode = 0x80010106

const mtaInitAttempts = 4

// AsyncStatus values from Windows.Foundation.
const (
	asyncStarted   = 0
	asyncCompleted = 1
	asyncCanceled  = 2
	asyncError     = 3
)

// AudioGraphSettings and node creation statuses: zero means Success.
const statusSuccess = 0

// isChangedMode reports whether err is RPC_E_CHANGED_MODE.
func isChangedMode(err error) bool {
	var hr win32.HRESULT
	return errors.As(err, &hr) && uint32(hr) == rpcEChangedMode
}

// guid builds a GUID from its canonical parts.
func guid(data1 uint32, data2, data3 uint16, d0, d1, d2, d3, d4, d5, d6, d7 byte) win32.GUID {
	return win32.GUID{Data1: data1, Data2: data2, Data3: data3, Data4: [8]byte{d0, d1, d2, d3, d4, d5, d6, d7}}
}

// Interface identifiers, from the Windows SDK metadata.
var (
	iidAsyncInfo                         = guid(0x00000036, 0x0000, 0x0000, 0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46)
	iidAudioGraph                        = guid(0x1ad46eed, 0xe48c, 0x4e14, 0x96, 0x60, 0x2c, 0x4f, 0x83, 0xe9, 0xcd, 0xd8)
	iidAudioGraph3                       = guid(0xddcd25ae, 0x1185, 0x42a7, 0x83, 0x1d, 0x6a, 0x9b, 0x0f, 0xc8, 0x68, 0x20)
	iidAudioGraphStatics                 = guid(0x76ec3132, 0xe159, 0x4ab7, 0xa8, 0x2a, 0x17, 0xbe, 0xb4, 0xb3, 0x1e, 0x94)
	iidAudioGraphSettingsFactory         = guid(0xa5d91cc6, 0xc2eb, 0x4a61, 0xa2, 0x14, 0x1d, 0x66, 0xd7, 0x5f, 0x83, 0xda)
	iidAudioGraphSettings                = guid(0x1d59647f, 0xe6fe, 0x4628, 0x84, 0xf8, 0x9d, 0x8b, 0xdb, 0xa2, 0x57, 0x85)
	iidAudioEncodingProperties           = guid(0x62bc7a16, 0x005c, 0x4b3b, 0x8a, 0x0b, 0x0a, 0x09, 0x0e, 0x96, 0x87, 0xf3)
	iidAudioEncodingPropertiesStatics    = guid(0x0cad332c, 0xebe9, 0x4527, 0xb3, 0x6d, 0xe4, 0x2a, 0x13, 0xcf, 0x38, 0xdb)
	iidMediaEncodingProperties           = guid(0xb4002af6, 0xacd4, 0x4e5a, 0xa2, 0x4b, 0x5d, 0x74, 0x98, 0xa8, 0xb8, 0xc4)
	iidMediaSourceStatics                = guid(0xf77d6fa4, 0x4652, 0x410e, 0xb1, 0xd8, 0xe9, 0xa5, 0xe2, 0x45, 0xa4, 0x5c)
	iidUriRuntimeClassFactory            = guid(0x44a9796f, 0x723e, 0x4fdf, 0xa2, 0x18, 0x03, 0x3e, 0x75, 0xb0, 0xc0, 0x84)
	iidCreateAudioGraphResult            = guid(0x5453ef7e, 0x7bde, 0x4b76, 0xbb, 0x5d, 0x48, 0xf7, 0x9c, 0xfc, 0x8c, 0x0b)
	iidCreateMediaSourceInputNodeResult  = guid(0x46a658a3, 0x53c0, 0x4d59, 0x9e, 0x51, 0xcc, 0x1d, 0x10, 0x44, 0xa4, 0xc4)
	iidCreateMediaSourceInputNodeResult2 = guid(0x63514ce8, 0x6a1a, 0x49e3, 0x97, 0xec, 0x28, 0xfd, 0x5b, 0xe1, 0x14, 0xe5)
	iidCreateAudioDeviceOutputNodeResult = guid(0xf7776d27, 0x1d9a, 0x47f7, 0x9c, 0xd4, 0x28, 0x59, 0xcc, 0x1b, 0x7b, 0xff)
	iidMediaSourceAudioInputNode         = guid(0x99d8983b, 0xa88a, 0x4041, 0x8e, 0x4f, 0xdd, 0xba, 0xc0, 0xc9, 0x1f, 0xd3)
	iidAudioInputNode                    = guid(0xd148005c, 0x8428, 0x4784, 0xb7, 0xfd, 0xa9, 0x9d, 0x46, 0x8c, 0x5d, 0x20)
	iidAudioNode                         = guid(0x15389d7f, 0xdbd8, 0x4819, 0xbf, 0x03, 0x66, 0x8e, 0x93, 0x57, 0xcd, 0x6d)
	iidAudioFrameOutputNode              = guid(0xb847371b, 0x3299, 0x45f5, 0x88, 0xb3, 0xc9, 0xd1, 0x2a, 0x3f, 0x1c, 0xc8)
	iidAudioFrame                        = guid(0xe36ac304, 0xaab2, 0x4277, 0x9e, 0xd0, 0x43, 0xce, 0xdf, 0x8e, 0x29, 0xc6)
	iidAudioBuffer                       = guid(0x35175827, 0x724b, 0x4c6a, 0xb1, 0x30, 0xf6, 0x53, 0x7f, 0x9a, 0xe0, 0xd0)
	iidMemoryBuffer                      = guid(0xfbc4dd2a, 0x245b, 0x11e4, 0xaf, 0x98, 0x68, 0x94, 0x23, 0x26, 0x0c, 0xf8)
	iidClosable                          = guid(0x30d5a829, 0x7fa4, 0x4026, 0x83, 0xbb, 0xd7, 0x5b, 0xae, 0x4e, 0xa9, 0x9e)
)

// hstring creates an HSTRING from a Go string. Length is in UTF-16 code
// units, not bytes: source URIs may contain non-ASCII path segments.
func hstring(s string) (rwinrt.HSTRING, error) {
	var h rwinrt.HSTRING
	n := len(utf16.Encode([]rune(s)))
	if err := rwinrt.WindowsCreateString(&s, uint32(n), &h); err != nil {
		return 0, err
	}
	return h, nil
}

func freeHString(h rwinrt.HSTRING) {
	if h != 0 {
		_ = rwinrt.WindowsDeleteString(h)
	}
}

// syncMTA runs fn on a dedicated thread initialized as MTA and returns when fn
// returns. RoInitialize is retried on a fresh goroutine if the current thread
// already belongs to another apartment.
func syncMTA(fn func() error) error {
	done := make(chan error, 1)
	var attempt func(int)
	attempt = func(n int) {
		if err := mtaAttempt(fn); err != nil {
			if isChangedMode(err) && n < mtaInitAttempts {
				go func() {
					time.Sleep(time.Millisecond)
					attempt(n + 1)
				}()
				return
			}
			done <- err
			return
		}
		done <- nil
	}
	go attempt(0)
	return <-done
}

// mtaAttempt locks the current thread, initializes it, and runs fn. It returns
// RPC_E_CHANGED_MODE without running fn when the thread cannot join the MTA.
// A panic inside fn is reported as an error: Probe must never take the host
// process down for a misbehaving WinRT component.
func mtaAttempt(fn func() error) (err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("winrt: %v", r)
		}
	}()
	if err := rwinrt.RoInitialize(rwinrt.RO_INIT_MULTITHREADED); err != nil {
		return err
	}
	defer rwinrt.RoUninitialize()
	return fn()
}

// activationFactory activates the WinRT class class and asks for the static
// interface iid. The returned object is a borrowed reference owned by the
// caller (Release it).
func activationFactory(class string, iid *win32.GUID) (*rwinrt.IInspectable, error) {
	h, err := hstring(class)
	if err != nil {
		return nil, err
	}
	defer freeHString(h)

	var unk *win32.IUnknown
	if err := rwinrt.RoGetActivationFactory(h, iid, &unk); err != nil {
		return nil, err
	}
	return (*rwinrt.IInspectable)(unsafe.Pointer(unk)), nil
}

// activateInstance creates a default instance of class.
func activateInstance(class string) (*rwinrt.IInspectable, error) {
	h, err := hstring(class)
	if err != nil {
		return nil, err
	}
	defer freeHString(h)

	var inst *rwinrt.IInspectable
	if err := rwinrt.RoActivateInstance(h, &inst); err != nil {
		return nil, err
	}
	return inst, nil
}

// as reinterprets an IInspectable pointer as the interface struct the
// activation factory was asked for. No AddRef: the reference transfers.
func as[T any](obj *rwinrt.IInspectable) *T {
	return (*T)(unsafe.Pointer(obj))
}

// outParam returns an out-pointer in the form SyscallN wants.
func outParam(p unsafe.Pointer) uintptr { return uintptr(win32.OutParam(p)) }

// iAsyncOperation is the ABI view of IAsyncOperation<T>.
//
// Unlike the generated projections, the ABI interface does not inherit the
// IAsyncInfo methods: its vtable is IInspectable(0-5), put_Completed(6),
// get_Completed(7), GetResults(8). Id/Status/ErrorCode/Cancel/Close live on a
// separate IAsyncInfo interface that the same object implements, reached by
// QueryInterface. Passing this pointer to GetResults at slot 12 used to land
// past the end of the vtable (and crashed).
//
// The result type T varies per call, so GetResults takes a raw destination.
type iAsyncOperation struct {
	rwinrt.IInspectable
}

// slot 8: GetResults(TResult*)
func (x *iAsyncOperation) getResults(out unsafe.Pointer) error {
	r1, _, _ := syscall.SyscallN(x.LpVtbl[8], uintptr(unsafe.Pointer(x)), outParam(out))
	return win32.ErrIfFailed(int32(r1))
}

// iAsyncInfo is Windows.Foundation.IAsyncInfo: IInspectable(0-5), get_Id(6),
// get_Status(7), get_ErrorCode(8), Cancel(9), Close(10).
type iAsyncInfo struct {
	rwinrt.IInspectable
}

func (x *iAsyncInfo) status() (int32, error) {
	var st int32
	r1, _, _ := syscall.SyscallN(x.LpVtbl[7], uintptr(unsafe.Pointer(x)), uintptr(unsafe.Pointer(&st)))
	return st, win32.ErrIfFailed(int32(r1))
}

func (x *iAsyncInfo) errorCode() int32 {
	var hr int32
	r1, _, _ := syscall.SyscallN(x.LpVtbl[8], uintptr(unsafe.Pointer(x)), uintptr(unsafe.Pointer(&hr)))
	if int32(r1) < 0 {
		return int32(r1)
	}
	return hr
}

func (x *iAsyncInfo) close() {
	_, _, _ = syscall.SyscallN(x.LpVtbl[10], uintptr(unsafe.Pointer(x)))
}

// awaitOp polls an IAsyncOperation until it completes and stores its result in
// out, then closes and releases the operation. A failed or canceled operation
// reports the underlying HRESULT.
func awaitOp(ctx context.Context, op *iAsyncOperation, out unsafe.Pointer) error {
	defer op.Release()

	info, err := win32.QueryInterface[iAsyncInfo](op, &iidAsyncInfo)
	if err != nil {
		return fmt.Errorf("winrt: IAsyncInfo: %w", err)
	}
	defer func() {
		info.close()
		info.Release()
	}()

	for {
		st, err := info.status()
		if err != nil {
			return err
		}
		switch st {
		case asyncCompleted:
			return op.getResults(out)
		case asyncCanceled:
			return fmt.Errorf("winrt: operation canceled")
		case asyncError:
			code := info.errorCode()
			if code >= 0 {
				code = int32(0x80004005 - 1<<32) // E_FAIL
			}
			return win32.HRESULT(code)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// The following are the ABI shapes of the WinRT interfaces this backend uses.
// Every Windows Runtime interface derives from IInspectable, so the first six
// vtable slots are shared and each declared method starts at slot 6. The
// methods are wrapped individually because the generated Go bindings in
// go-bindings-win32 do not cover Windows.Media.Audio.

type iAudioGraph struct{ rwinrt.IInspectable }

// slot 12: CreateFrameOutputNodeWithFormat(IAudioEncodingProperties*)
func (x *iAudioGraph) createFrameOutputNodeWithFormat(enc *iAudioEncodingProperties, out **iAudioFrameOutputNode) error {
	r1, _, _ := syscall.SyscallN(x.LpVtbl[12], uintptr(unsafe.Pointer(x)), uintptr(unsafe.Pointer(enc)), outParam(unsafe.Pointer(out)))
	return win32.ErrIfFailed(int32(r1))
}

// slot 13: CreateDeviceOutputNodeAsync()
func (x *iAudioGraph) createDeviceOutputNodeAsync(out **iAsyncOperation) error {
	r1, _, _ := syscall.SyscallN(x.LpVtbl[13], uintptr(unsafe.Pointer(x)), outParam(unsafe.Pointer(out)))
	return win32.ErrIfFailed(int32(r1))
}

// slot 19: Start()
func (x *iAudioGraph) start() error {
	r1, _, _ := syscall.SyscallN(x.LpVtbl[19], uintptr(unsafe.Pointer(x)))
	return win32.ErrIfFailed(int32(r1))
}

// slot 20: Stop()
func (x *iAudioGraph) stop() error {
	r1, _, _ := syscall.SyscallN(x.LpVtbl[20], uintptr(unsafe.Pointer(x)))
	return win32.ErrIfFailed(int32(r1))
}

type iAudioGraph3 struct{ rwinrt.IInspectable }

// slot 6: CreateMediaSourceAudioInputNodeAsync(IMediaSource*)
func (x *iAudioGraph3) createMediaSourceAudioInputNodeAsync(src *iMediaSource, out **iAsyncOperation) error {
	r1, _, _ := syscall.SyscallN(x.LpVtbl[6], uintptr(unsafe.Pointer(x)), uintptr(unsafe.Pointer(src)), outParam(unsafe.Pointer(out)))
	return win32.ErrIfFailed(int32(r1))
}

type iAudioGraphStatics struct{ rwinrt.IInspectable }

// slot 6: CreateAsync(AudioGraphSettings)
func (x *iAudioGraphStatics) createAsync(settings *iAudioGraphSettings, out **iAsyncOperation) error {
	r1, _, _ := syscall.SyscallN(x.LpVtbl[6], uintptr(unsafe.Pointer(x)), uintptr(unsafe.Pointer(settings)), outParam(unsafe.Pointer(out)))
	return win32.ErrIfFailed(int32(r1))
}

type iAudioGraphSettingsFactory struct{ rwinrt.IInspectable }

// slot 6: Create(AudioRenderCategory, out AudioGraphSettings)
func (x *iAudioGraphSettingsFactory) create(category int32, out **iAudioGraphSettings) error {
	r1, _, _ := syscall.SyscallN(x.LpVtbl[6], uintptr(unsafe.Pointer(x)), uintptr(category), outParam(unsafe.Pointer(out)))
	return win32.ErrIfFailed(int32(r1))
}

type iAudioGraphSettings struct{ rwinrt.IInspectable }

// slot 7: put_EncodingProperties(IAudioEncodingProperties*)
func (x *iAudioGraphSettings) setEncodingProperties(enc *iAudioEncodingProperties) error {
	r1, _, _ := syscall.SyscallN(x.LpVtbl[7], uintptr(unsafe.Pointer(x)), uintptr(unsafe.Pointer(enc)))
	return win32.ErrIfFailed(int32(r1))
}

type iAudioEncodingPropertiesStatics struct{ rwinrt.IInspectable }

// slot 9: CreatePcm(sampleRate, channelCount, bitsPerSample)
func (x *iAudioEncodingPropertiesStatics) createPcm(rate, channels, bits uint32, out **iAudioEncodingProperties) error {
	r1, _, _ := syscall.SyscallN(x.LpVtbl[9], uintptr(unsafe.Pointer(x)), uintptr(rate), uintptr(channels), uintptr(bits), outParam(unsafe.Pointer(out)))
	return win32.ErrIfFailed(int32(r1))
}

type iAudioEncodingProperties struct{ rwinrt.IInspectable }

func (x *iAudioEncodingProperties) setBitrate(v uint32) error { // slot 6
	r1, _, _ := syscall.SyscallN(x.LpVtbl[6], uintptr(unsafe.Pointer(x)), uintptr(v))
	return win32.ErrIfFailed(int32(r1))
}

func (x *iAudioEncodingProperties) setChannelCount(v uint32) error { // slot 8
	r1, _, _ := syscall.SyscallN(x.LpVtbl[8], uintptr(unsafe.Pointer(x)), uintptr(v))
	return win32.ErrIfFailed(int32(r1))
}

func (x *iAudioEncodingProperties) channelCount() (uint32, error) { // slot 9
	var v uint32
	r1, _, _ := syscall.SyscallN(x.LpVtbl[9], uintptr(unsafe.Pointer(x)), uintptr(unsafe.Pointer(&v)))
	return v, win32.ErrIfFailed(int32(r1))
}

func (x *iAudioEncodingProperties) setSampleRate(v uint32) error { // slot 10
	r1, _, _ := syscall.SyscallN(x.LpVtbl[10], uintptr(unsafe.Pointer(x)), uintptr(v))
	return win32.ErrIfFailed(int32(r1))
}

func (x *iAudioEncodingProperties) sampleRate() (uint32, error) { // slot 11
	var v uint32
	r1, _, _ := syscall.SyscallN(x.LpVtbl[11], uintptr(unsafe.Pointer(x)), uintptr(unsafe.Pointer(&v)))
	return v, win32.ErrIfFailed(int32(r1))
}

func (x *iAudioEncodingProperties) setBitsPerSample(v uint32) error { // slot 12
	r1, _, _ := syscall.SyscallN(x.LpVtbl[12], uintptr(unsafe.Pointer(x)), uintptr(v))
	return win32.ErrIfFailed(int32(r1))
}

type iMediaEncodingProperties struct{ rwinrt.IInspectable }

// slot 8: put_Subtype(HSTRING)
func (x *iMediaEncodingProperties) setSubtype(subtype rwinrt.HSTRING) error {
	r1, _, _ := syscall.SyscallN(x.LpVtbl[8], uintptr(unsafe.Pointer(x)), uintptr(subtype))
	return win32.ErrIfFailed(int32(r1))
}

type iUriRuntimeClassFactory struct{ rwinrt.IInspectable }

// slot 6: CreateUri(HSTRING)
func (x *iUriRuntimeClassFactory) createUri(uri rwinrt.HSTRING, out **iUriRuntimeClass) error {
	r1, _, _ := syscall.SyscallN(x.LpVtbl[6], uintptr(unsafe.Pointer(x)), uintptr(uri), outParam(unsafe.Pointer(out)))
	return win32.ErrIfFailed(int32(r1))
}

type iMediaSourceStatics struct{ rwinrt.IInspectable }

// slot 13: CreateFromUri(Uri)
func (x *iMediaSourceStatics) createFromUri(uri *iUriRuntimeClass, out **iMediaSource) error {
	r1, _, _ := syscall.SyscallN(x.LpVtbl[13], uintptr(unsafe.Pointer(x)), uintptr(unsafe.Pointer(uri)), outParam(unsafe.Pointer(out)))
	return win32.ErrIfFailed(int32(r1))
}

type iCreateAudioGraphResult struct{ rwinrt.IInspectable }

func (x *iCreateAudioGraphResult) status() (int32, error) { // slot 6
	var v int32
	r1, _, _ := syscall.SyscallN(x.LpVtbl[6], uintptr(unsafe.Pointer(x)), uintptr(unsafe.Pointer(&v)))
	return v, win32.ErrIfFailed(int32(r1))
}

func (x *iCreateAudioGraphResult) graph(out **iAudioGraph) error { // slot 7
	r1, _, _ := syscall.SyscallN(x.LpVtbl[7], uintptr(unsafe.Pointer(x)), outParam(unsafe.Pointer(out)))
	return win32.ErrIfFailed(int32(r1))
}

type iCreateMediaSourceInputNodeResult struct{ rwinrt.IInspectable }

func (x *iCreateMediaSourceInputNodeResult) status() (int32, error) { // slot 6
	var v int32
	r1, _, _ := syscall.SyscallN(x.LpVtbl[6], uintptr(unsafe.Pointer(x)), uintptr(unsafe.Pointer(&v)))
	return v, win32.ErrIfFailed(int32(r1))
}

func (x *iCreateMediaSourceInputNodeResult) node(out **iMediaSourceAudioInputNode) error { // slot 7
	r1, _, _ := syscall.SyscallN(x.LpVtbl[7], uintptr(unsafe.Pointer(x)), outParam(unsafe.Pointer(out)))
	return win32.ErrIfFailed(int32(r1))
}

// iCreateMediaSourceInputNodeResult2 adds ExtendedError, the HRESULT behind a
// non-Success creation status.
type iCreateMediaSourceInputNodeResult2 struct{ rwinrt.IInspectable }

func (x *iCreateMediaSourceInputNodeResult2) extendedError(out *int32) error { // slot 6
	r1, _, _ := syscall.SyscallN(x.LpVtbl[6], uintptr(unsafe.Pointer(x)), uintptr(unsafe.Pointer(out)))
	return win32.ErrIfFailed(int32(r1))
}

type iCreateAudioDeviceOutputNodeResult struct{ rwinrt.IInspectable }

func (x *iCreateAudioDeviceOutputNodeResult) status() (int32, error) { // slot 6
	var v int32
	r1, _, _ := syscall.SyscallN(x.LpVtbl[6], uintptr(unsafe.Pointer(x)), uintptr(unsafe.Pointer(&v)))
	return v, win32.ErrIfFailed(int32(r1))
}

func (x *iCreateAudioDeviceOutputNodeResult) deviceOutputNode(out **iAudioDeviceOutputNode) error { // slot 7
	r1, _, _ := syscall.SyscallN(x.LpVtbl[7], uintptr(unsafe.Pointer(x)), outParam(unsafe.Pointer(out)))
	return win32.ErrIfFailed(int32(r1))
}

type iMediaSourceAudioInputNode struct{ rwinrt.IInspectable }

func (x *iMediaSourceAudioInputNode) seek(ticks int64) error { // slot 9
	r1, _, _ := syscall.SyscallN(x.LpVtbl[9], uintptr(unsafe.Pointer(x)), uintptr(uint64(ticks)))
	return win32.ErrIfFailed(int32(r1))
}

func (x *iMediaSourceAudioInputNode) duration() (int64, error) { // slot 16
	var v int64
	r1, _, _ := syscall.SyscallN(x.LpVtbl[16], uintptr(unsafe.Pointer(x)), uintptr(unsafe.Pointer(&v)))
	return v, win32.ErrIfFailed(int32(r1))
}

type iAudioInputNode struct{ rwinrt.IInspectable }

// slot 7: AddOutgoingConnection(IAudioNode*)
func (x *iAudioInputNode) addOutgoingConnection(node unsafe.Pointer) error {
	r1, _, _ := syscall.SyscallN(x.LpVtbl[7], uintptr(unsafe.Pointer(x)), uintptr(node))
	return win32.ErrIfFailed(int32(r1))
}

type iAudioNode struct{ rwinrt.IInspectable }

func (x *iAudioNode) encodingProperties(out **iAudioEncodingProperties) error { // slot 9
	r1, _, _ := syscall.SyscallN(x.LpVtbl[9], uintptr(unsafe.Pointer(x)), outParam(unsafe.Pointer(out)))
	return win32.ErrIfFailed(int32(r1))
}

func (x *iAudioNode) reset() error { // slot 14
	r1, _, _ := syscall.SyscallN(x.LpVtbl[14], uintptr(unsafe.Pointer(x)))
	return win32.ErrIfFailed(int32(r1))
}

type iAudioFrameOutputNode struct{ rwinrt.IInspectable }

// slot 6: GetFrame()
func (x *iAudioFrameOutputNode) getFrame(out **iAudioFrame) error {
	r1, _, _ := syscall.SyscallN(x.LpVtbl[6], uintptr(unsafe.Pointer(x)), outParam(unsafe.Pointer(out)))
	return win32.ErrIfFailed(int32(r1))
}

type iAudioFrame struct{ rwinrt.IInspectable }

// slot 6: LockBuffer(AudioBufferAccessMode, out AudioBuffer)
func (x *iAudioFrame) lockBuffer(mode int32, out **iAudioBuffer) error {
	r1, _, _ := syscall.SyscallN(x.LpVtbl[6], uintptr(unsafe.Pointer(x)), uintptr(mode), outParam(unsafe.Pointer(out)))
	return win32.ErrIfFailed(int32(r1))
}

type iAudioBuffer struct{ rwinrt.IInspectable }

func (x *iAudioBuffer) length() (uint32, error) { // slot 7
	var v uint32
	r1, _, _ := syscall.SyscallN(x.LpVtbl[7], uintptr(unsafe.Pointer(x)), uintptr(unsafe.Pointer(&v)))
	return v, win32.ErrIfFailed(int32(r1))
}

type iMemoryBuffer struct{ rwinrt.IInspectable }

// slot 6: CreateReference()
func (x *iMemoryBuffer) createReference(out **iMemoryBufferReference) error {
	r1, _, _ := syscall.SyscallN(x.LpVtbl[6], uintptr(unsafe.Pointer(x)), outParam(unsafe.Pointer(out)))
	return win32.ErrIfFailed(int32(r1))
}

type iMemoryBufferReference struct{ rwinrt.IInspectable }

type iClosable struct{ rwinrt.IInspectable }

func (x *iClosable) close() { // slot 6
	_, _, _ = syscall.SyscallN(x.LpVtbl[6], uintptr(unsafe.Pointer(x)))
}

// closeObject closes an object that implements IClosable, ignoring objects
// that do not.
func closeObject(obj *rwinrt.IInspectable) {
	c, err := win32.QueryInterface[iClosable](obj, &iidClosable)
	if err != nil {
		return
	}
	c.close()
	c.Release()
}

// Placeholder types named after the Windows Runtime classes they carry; they
// only exist to make signatures self-documenting.
type iUriRuntimeClass struct{ rwinrt.IInspectable }
type iMediaSource struct{ rwinrt.IInspectable }
type iAudioDeviceOutputNode struct{ rwinrt.IInspectable }
