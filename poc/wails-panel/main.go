package main

import (
	"embed"
	"os"
	"strings"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	mode := modePanel
	accessory := false

	// PoC 的旋钮：-mode=baseline|mask|panel   -accessory
	for _, a := range os.Args[1:] {
		switch {
		case a == "-mode=baseline":
			mode = modeBaseline
		case a == "-mode=mask":
			mode = modeMaskOnly
		case a == "-mode=panel":
			mode = modePanel
		case a == "-mode=panelmin":
			mode = modePanelMin
		case a == "-mode=adopt":
			mode = modeAdopt
		case a == "-accessory":
			accessory = true
		}
	}
	if v := os.Getenv("PAWCLIP_PROBE_MODE"); v != "" {
		switch strings.ToLower(v) {
		case "baseline":
			mode = modeBaseline
		case "mask":
			mode = modeMaskOnly
		case "panel":
			mode = modePanel
		case "panelmin":
			mode = modePanelMin
		case "adopt":
			mode = modeAdopt
		}
	}
	if os.Getenv("PAWCLIP_PROBE_ACCESSORY") == "1" {
		accessory = true
	}

	app := NewApp(mode, accessory)

	err := wails.Run(&options.App{
		Title:       "PawClip M0 Probe",
		Width:       560,
		Height:      400,
		Frameless:   true,
		AlwaysOnTop: true,
		AssetServer: &assetserver.Options{Assets: assets},
		// 面板底色贴近 PawClip 主图的浅灰卡片
		BackgroundColour: &options.RGBA{R: 244, G: 244, B: 246, A: 1},
		OnStartup:        app.startup,
		Bind:             []interface{}{app},
		Mac: &mac.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}
