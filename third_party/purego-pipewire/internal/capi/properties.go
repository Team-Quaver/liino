package capi

import (
	"runtime"
	"unsafe"

	"github.com/bnema/purego"

	portout "github.com/bnema/purego-pipewire/internal/ports/out"
)

// pw_stream_new_simple takes a struct pw_properties *; upstream passed nil, so
// PipeWire named the client after the host process (Electron showed up as
// "electron" in every mixer). This binding lets us hand it a spa_dict without
// pulling in the variadic pw_properties_new.
//
//	struct spa_dict {
//	    uint32_t flags;
//	    uint32_t n_items;
//	    const struct spa_dict_item *items;
//	};
//
// struct spa_dict_item { const char *key; const char *value; };
type spaDictItem struct {
	key   *byte
	value *byte
}

type spaDict struct {
	flags  uint32
	nItems uint32
	items  *spaDictItem
}

type pwPropertiesNewDictFunc func(dict unsafe.Pointer) unsafe.Pointer

var pwPropertiesNewDict pwPropertiesNewDictFunc

func registerProperties(handle uintptr) {
	purego.RegisterLibFunc(&pwPropertiesNewDict, handle, "pw_properties_new_dict")
}

func cString(s string) []byte {
	return append([]byte(s), 0)
}

// buildStreamProperties returns a pw_properties pointer for pw_stream_new_simple
// (the stream takes ownership), or nil when nothing was requested. All backing
// storage is kept alive until the call has copied the values.
func buildStreamProperties(props portout.StreamProperties, fallbackName string) unsafe.Pointer {
	if pwPropertiesNewDict == nil {
		return nil
	}
	items := make([]spaDictItem, 0, 6)
	keep := make([][]byte, 0, 12)
	add := func(key, value string) {
		if value == "" {
			return
		}
		kb := cString(key)
		vb := cString(value)
		keep = append(keep, kb, vb)
		items = append(items, spaDictItem{key: &kb[0], value: &vb[0]})
	}
	streamName := props.StreamName
	if streamName == "" {
		streamName = fallbackName
	}
	add("application.name", props.AppName)
	add("application.process.binary", props.AppName)
	add("media.name", streamName)
	add("node.name", props.AppName)
	add("node.description", props.AppName)
	add("media.role", props.MediaRole)
	if len(items) == 0 {
		return nil
	}
	dict := spaDict{nItems: uint32(len(items)), items: &items[0]}
	ptr := pwPropertiesNewDict(unsafe.Pointer(&dict))
	runtime.KeepAlive(items)
	runtime.KeepAlive(keep)
	runtime.KeepAlive(&dict)
	return ptr
}
