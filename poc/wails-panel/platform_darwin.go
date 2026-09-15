//go:build darwin

package main

/*
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>
#include "panel_darwin.h"
*/
import "C"

import (
	"errors"
	"unsafe"
)

const (
	modeBaseline = 0
	modeMaskOnly = 1
	modePanel    = 2
	modePanelMin = 3
	modeAdopt    = 4
)

func platformHasWindow() bool { return C.pawclip_has_window() == 1 }

func platformSetup(mode int, accessory bool) error {
	var errbuf [512]C.char
	acc := C.int(0)
	if accessory {
		acc = C.int(1)
	}
	rc := C.pawclip_setup(C.int(mode), acc, &errbuf[0], C.int(len(errbuf)))
	if rc != 0 {
		return errors.New(C.GoString(&errbuf[0]))
	}
	return nil
}

func platformHide() { C.pawclip_hide() }

func platformShowNoActivate() bool { return C.pawclip_show_noactivate() == 1 }

func platformActivateApp() { C.pawclip_activate_app() }

func platformMakeKey() bool { return C.pawclip_make_key() == 1 }

func platformDiag() string {
	p := C.pawclip_diag_json()
	if p == nil {
		return "{}"
	}
	defer C.pawclip_free(p)

	_ = unsafe.Pointer(p) // 指针由 C 分配，Go 侧只读
	return C.GoString(p)
}
