//go:build !darwin

package main

import "errors"

const (
	modeBaseline = 0
	modeMaskOnly = 1
	modePanel    = 2
	modePanelMin = 3
	modeAdopt    = 4
)

func platformHasWindow() bool                      { return true }
func platformSetup(mode int, accessory bool) error { return errors.New("darwin only") }
func platformHide()                                {}
func platformShowNoActivate() bool                 { return false }
func platformActivateApp()                         {}
func platformMakeKey() bool                        { return false }
func platformDiag() string                         { return "{}" }
