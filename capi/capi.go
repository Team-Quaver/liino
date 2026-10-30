//go:build liino_capi

// Package capi exports Liino as a C ABI shared library for Electron.
//
// Build it with:
//
//	go build -buildmode=c-shared -tags liino_capi -o libliino.so ./capi
//
// The build tag is deliberate: the whole audio pipeline is pure Go and is
// verified with CGO_ENABLED=0. Only this packaging layer needs a C linker, so
// the tag keeps it optional. Node loads the result with an FFI binding
// (see node/) and never touches WASM.
package main

/*
#include <stdlib.h>
#include <string.h>
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
	"unsafe"

	"github.com/team-quaver/liino"
	"github.com/team-quaver/liino/decode"
	"github.com/team-quaver/liino/dsp"
	"github.com/team-quaver/liino/output"
)

// Error codes returned to the caller; see liino.h.
const (
	codeOK       C.int = 0
	codeInvalid  C.int = -1
	codeNotReady C.int = -2
	codeClosed   C.int = -3
	codeInternal C.int = -4
)

// handle is one exported player plus its event subscription.
type handle struct {
	player *liino.Player
	events <-chan liino.Event
	cancel func()
}

var (
	handlesMu sync.Mutex
	handles   = map[*C.struct_liino_player]*handle{}
	nextID    = uintptr(1)
)

func register(h *handle) *C.struct_liino_player {
	handlesMu.Lock()
	defer handlesMu.Unlock()
	// A stable non-nil pointer is enough: Go never dereferences it.
	key := (*C.struct_liino_player)(unsafe.Pointer(nextID))
	nextID += unsafe.Sizeof(uintptr(0))
	handles[key] = h
	return key
}

func lookup(key *C.struct_liino_player) (*handle, bool) {
	handlesMu.Lock()
	defer handlesMu.Unlock()
	h, ok := handles[key]
	return h, ok
}

func cString(v any) *C.char {
	data, err := json.Marshal(v)
	if err != nil {
		data = []byte(`{"error":"json"}`)
	}
	return C.CString(string(data))
}

//export liino_version
func liino_version() *C.char { return C.CString(liino.Version) }

//export liino_diagnostics
func liino_diagnostics(out **C.char) C.int {
	if out == nil {
		return codeInvalid
	}
	*out = cString(liino.Diagnose())
	return codeOK
}

//export liino_devices
func liino_devices(out **C.char) C.int {
	if out == nil {
		return codeInvalid
	}
	devs, err := liino.AllDevices()
	if err != nil && len(devs) == 0 {
		*out = cString(map[string]any{"devices": []output.Device{}, "error": err.Error()})
		return codeOK
	}
	*out = cString(map[string]any{"devices": devs})
	return codeOK
}

//export liino_create
func liino_create(configJSON *C.char, errp **C.char) *C.struct_liino_player {
	cfg := liino.Config{AppName: "Quaver Music"}
	if configJSON != nil {
		var in struct {
			Decoder     string  `json:"decoder"`
			Driver      string  `json:"driver"`
			DeviceID    string  `json:"device_id"`
			AppName     string  `json:"app_name"`
			StreamName  string  `json:"stream_name"`
			RingSeconds float64 `json:"ring_seconds"`
			PrerollMS   int     `json:"preroll_ms"`
			PeriodMS    int     `json:"period_ms"`
			FadeOutMS   int     `json:"fade_out_ms"`
			Volume      float64 `json:"volume"`
			Limiter     bool    `json:"limiter"`
			LimiterDB   float64 `json:"limiter_db"`
			EQ          []struct {
				Freq  float64 `json:"freq"`
				Q     float64 `json:"q"`
				Gain  float64 `json:"gain"`
				Shelf string  `json:"shelf"`
			} `json:"eq"`
		}
		raw := C.GoString(configJSON)
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &in); err != nil {
				return failHandle(errp, err)
			}
		}
		cfg.Decoder = in.Decoder
		cfg.Driver = in.Driver
		cfg.DeviceID = in.DeviceID
		if in.AppName != "" {
			cfg.AppName = in.AppName
		}
		cfg.StreamName = in.StreamName
		if in.RingSeconds > 0 {
			cfg.RingFrames = int(in.RingSeconds * 48000)
		}
		if in.PrerollMS > 0 {
			cfg.PrerollFrames = in.PrerollMS * 48000 / 1000
		}
		if in.PeriodMS > 0 {
			cfg.PeriodFrames = in.PeriodMS * 48000 / 1000
		}
		cfg.FadeOut = time.Duration(in.FadeOutMS) * time.Millisecond
		if in.Volume > 0 {
			cfg.Volume = in.Volume
		}
		cfg.Limiter = in.Limiter
		cfg.LimiterDB = in.LimiterDB
		for _, b := range in.EQ {
			cfg.EQ = append(cfg.EQ, dsp.Config{Freq: b.Freq, Q: b.Q, Gain: b.Gain, Shelf: b.Shelf})
		}
	}

	player, err := liino.New(cfg)
	if err != nil {
		return failHandle(errp, err)
	}
	events, cancel := player.Events(256)
	h := &handle{player: player, events: events, cancel: cancel}
	return register(h)
}

func failHandle(errp **C.char, err error) *C.struct_liino_player {
	if errp != nil && err != nil {
		*errp = C.CString(err.Error())
	}
	return nil
}

//export liino_destroy
func liino_destroy(key *C.struct_liino_player) {
	handlesMu.Lock()
	h, ok := handles[key]
	delete(handles, key)
	handlesMu.Unlock()
	if !ok {
		return
	}
	h.cancel()
	_ = h.player.Close()
}

//export liino_open
func liino_open(key *C.struct_liino_player, uri *C.char, optionsJSON *C.char, errp **C.char) C.int {
	h, ok := lookup(key)
	if !ok {
		return codeClosed
	}
	if uri == nil {
		return codeInvalid
	}
	var opts struct {
		StartMS  int64  `json:"start_ms"`
		AutoPlay bool   `json:"auto_play"`
		Format   string `json:"format"`
		Title    string `json:"title"`
	}
	if optionsJSON != nil {
		if raw := C.GoString(optionsJSON); raw != "" {
			if err := json.Unmarshal([]byte(raw), &opts); err != nil {
				return report(errp, err)
			}
		}
	}
	src := decode.Source{URI: C.GoString(uri), Format: opts.Format, Title: opts.Title}
	err := h.player.Open(context.Background(), src, liino.OpenOptions{
		StartAt:  time.Duration(opts.StartMS) * time.Millisecond,
		AutoPlay: opts.AutoPlay,
	})
	if err != nil {
		return report(errp, err)
	}
	return codeOK
}

//export liino_play
func liino_play(key *C.struct_liino_player, errp **C.char) C.int {
	return call(key, errp, func(h *handle) error { return h.player.Play() })
}

//export liino_pause
func liino_pause(key *C.struct_liino_player, errp **C.char) C.int {
	return call(key, errp, func(h *handle) error { return h.player.Pause() })
}

//export liino_stop
func liino_stop(key *C.struct_liino_player, errp **C.char) C.int {
	return call(key, errp, func(h *handle) error { return h.player.Stop() })
}

//export liino_seek_ms
func liino_seek_ms(key *C.struct_liino_player, ms C.int64_t, errp **C.char) C.int {
	return call(key, errp, func(h *handle) error {
		return h.player.Seek(time.Duration(int64(ms)) * time.Millisecond)
	})
}

//export liino_set_volume
func liino_set_volume(key *C.struct_liino_player, volume C.double, errp **C.char) C.int {
	return call(key, errp, func(h *handle) error {
		h.player.SetVolume(float64(volume))
		return nil
	})
}

//export liino_set_muted
func liino_set_muted(key *C.struct_liino_player, muted C.int, errp **C.char) C.int {
	return call(key, errp, func(h *handle) error {
		h.player.SetMuted(muted != 0)
		return nil
	})
}

//export liino_set_eq
func liino_set_eq(key *C.struct_liino_player, bandsJSON *C.char, errp **C.char) C.int {
	h, ok := lookup(key)
	if !ok {
		return codeClosed
	}
	if bandsJSON == nil {
		return report(errp, errors.New("capi: bands json required"))
	}
	var bands []struct {
		Freq  float64 `json:"freq"`
		Q     float64 `json:"q"`
		Gain  float64 `json:"gain"`
		Shelf string  `json:"shelf"`
	}
	if err := json.Unmarshal([]byte(C.GoString(bandsJSON)), &bands); err != nil {
		return report(errp, err)
	}
	configs := make([]dsp.Config, 0, len(bands))
	for _, b := range bands {
		configs = append(configs, dsp.Config{Freq: b.Freq, Q: b.Q, Gain: b.Gain, Shelf: b.Shelf})
	}
	if err := h.player.SetEQ(configs); err != nil {
		return report(errp, err)
	}
	return codeOK
}

//export liino_set_limiter
func liino_set_limiter(key *C.struct_liino_player, enabled C.int, errp **C.char) C.int {
	return call(key, errp, func(h *handle) error {
		h.player.SetLimiter(enabled != 0)
		return nil
	})
}

//export liino_set_bypass
func liino_set_bypass(key *C.struct_liino_player, bypass C.int, errp **C.char) C.int {
	return call(key, errp, func(h *handle) error {
		h.player.SetBypass(bypass != 0)
		return nil
	})
}

//export liino_set_device
func liino_set_device(key *C.struct_liino_player, deviceID *C.char, errp **C.char) C.int {
	h, ok := lookup(key)
	if !ok {
		return codeClosed
	}
	id := ""
	if deviceID != nil {
		id = C.GoString(deviceID)
	}
	if err := h.player.SetDevice(id); err != nil {
		return report(errp, err)
	}
	return codeOK
}

//export liino_snapshot_json
func liino_snapshot_json(key *C.struct_liino_player, out **C.char) C.int {
	h, ok := lookup(key)
	if !ok || out == nil {
		return codeClosed
	}
	*out = cString(h.player.Snapshot().JSON())
	return codeOK
}

//export liino_clock_json
func liino_clock_json(key *C.struct_liino_player, out **C.char) C.int {
	h, ok := lookup(key)
	if !ok || out == nil {
		return codeClosed
	}
	clock := h.player.Clock()
	*out = cString(map[string]any{
		"position_ms": clock.Position.Milliseconds(),
		"buffer_pos_ms": clock.BufferPos.Milliseconds(),
		"latency_ms": clock.Latency.Milliseconds(),
		"latency_ns": clock.Latency.Nanoseconds(),
		"monotonic_ns": clock.Monotonic,
		"frames": clock.Frames,
		"sample_rate": clock.SampleRate,
	})
	return codeOK
}

//export liino_level_json
func liino_level_json(key *C.struct_liino_player, out **C.char) C.int {
	h, ok := lookup(key)
	if !ok || out == nil {
		return codeClosed
	}
	level := h.player.Level()
	*out = cString(map[string]any{
		"peak_l": level.PeakL, "peak_r": level.PeakR,
		"rms_l": level.RMSL, "rms_r": level.RMSR,
		"clips": level.Clips, "frames": level.Frames,
	})
	return codeOK
}

//export liino_next_event
func liino_next_event(key *C.struct_liino_player, out **C.char) C.int {
	h, ok := lookup(key)
	if !ok || out == nil {
		return codeClosed
	}
	select {
	case ev, alive := <-h.events:
		if !alive {
			return codeClosed
		}
		*out = cString(ev)
		return codeOK
	default:
		return codeNotReady
	}
}

//export liino_wait_event
func liino_wait_event(key *C.struct_liino_player, timeoutMS C.int, out **C.char) C.int {
	h, ok := lookup(key)
	if !ok || out == nil {
		return codeClosed
	}
	timer := time.NewTimer(time.Duration(int32(timeoutMS)) * time.Millisecond)
	defer timer.Stop()
	select {
	case ev, alive := <-h.events:
		if !alive {
			return codeClosed
		}
		*out = cString(ev)
		return codeOK
	case <-timer.C:
		return codeNotReady
	}
}

//export liino_free_string
func liino_free_string(ptr *C.char) {
	if ptr != nil {
		C.free(unsafe.Pointer(ptr))
	}
}

func call(key *C.struct_liino_player, errp **C.char, fn func(*handle) error) C.int {
	h, ok := lookup(key)
	if !ok {
		return codeClosed
	}
	if err := fn(h); err != nil {
		return report(errp, err)
	}
	return codeOK
}

func report(errp **C.char, err error) C.int {
	if errp != nil && err != nil {
		*errp = C.CString(err.Error())
	}
	switch {
	case errors.Is(err, liino.ErrClosed):
		return codeClosed
	case errors.Is(err, liino.ErrNoMedia), errors.Is(err, liino.ErrNotRunning), errors.Is(err, liino.ErrSeekUnsupported):
		return codeNotReady
	case errors.Is(err, liino.ErrInvalidConfig):
		return codeInvalid
	default:
		return codeInternal
	}
}

// main exists only because -buildmode=c-shared requires package main.
func main() {}
