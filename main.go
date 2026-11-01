package main

import (
	"embed"
	"os"
	"slices"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"awgpilot/internal/service"
)

//go:embed all:frontend/dist
var assets embed.FS

const (
	windowWidth  = 640
	windowHeight = 720
)

func main() {
	// The same exe is both the window and the privileged service registered by the installer.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--service":
			if err := service.Run(appVersion); err != nil {
				os.Exit(1)
			}
			return
		case "--service-console": // development only, from an elevated console
			if err := service.RunConsole(appVersion); err != nil {
				println("Ошибка:", err.Error())
				os.Exit(1)
			}
			return
		case "--widget":
			runWidget()
			return
		}
	}

	app := NewApp()

	err := wails.Run(&options.App{
		Title:  "AWG Pilot " + appVersion,
		Width:  windowWidth,
		Height: windowHeight,
		// The edges can be dragged by up to 30% either way; maximizing is switched off in
		// startup (no maximize button, no double-click on the title bar, no snap to full screen).
		MinWidth:  windowWidth * 7 / 10,
		MinHeight: windowHeight * 7 / 10,
		MaxWidth:  windowWidth * 13 / 10,
		MaxHeight: windowHeight * 13 / 10,
		// Autostart passes --tray: start hidden, the tray icon is the only UI until opened.
		StartHidden: slices.Contains(os.Args[1:], "--tray"),
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 16, G: 20, B: 27, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		OnBeforeClose:    app.beforeClose,
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               "awgpilot-6f1c9a52-3b7e-4d21",
			OnSecondInstanceLaunch: func(options.SecondInstanceData) { app.ShowWindow() },
		},
		Windows: &windows.Options{Theme: windows.SystemDefault},
		// Widget is bound here too only so Wails generates its frontend bindings.
		Bind: []interface{}{app, &Widget{}},
	})
	if err != nil {
		println("Ошибка:", err.Error())
	}
}

// runWidget shows the small status window (started by the main app from the tray menu).
func runWidget() {
	w := &Widget{}
	err := wails.Run(&options.App{
		Title:            widgetTitle,
		Width:            widgetWidth,
		Height:           widgetHeight,
		DisableResize:    true,
		Frameless:        true,
		AlwaysOnTop:      true,
		AssetServer:      &assetserver.Options{Assets: assets},
		BackgroundColour: &options.RGBA{R: 0, G: 0, B: 0, A: 0}, // see-through: only the graph and text are drawn
		OnStartup:        w.startup,
		OnDomReady:       w.domReady,
		OnShutdown:       w.shutdown,
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "awgpilot-widget-6f1c9a52",
		},
		// Translucent + transparent webview: the desktop shows through wherever the page draws
		// nothing (WS_EX_NOREDIRECTIONBITMAP); no backdrop blur and no frame shadow around it.
		Windows: &windows.Options{Theme: windows.Dark, WebviewIsTransparent: true, WindowIsTranslucent: true,
			BackdropType: windows.None, DisableFramelessWindowDecorations: true},
		Bind: []interface{}{w},
	})
	if err != nil {
		println("Ошибка:", err.Error())
	}
}
