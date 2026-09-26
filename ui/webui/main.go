package main

import (
	"embed"
	"log/slog"
	"os"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed all:frontend/dist
var assets embed.FS

// Application identity. These are the values a packaged build shows in the
// window title, the about box and the dock, so they name the product rather
// than the framework.
const (
	appName        = "Player"
	appDescription = "A desktop audio player for the files on this machine"
)

// Window names, so the service can find a window it did not create and the
// frontend can tell the main window from the effect window.
const (
	mainWindowName   = "main"
	effectWindowName = "effects"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Registering the payload types is what gives the frontend a typed event
	// envelope; an unregistered name still travels, but untyped.
	application.RegisterEvent[Snapshot](eventSnapshot)
	application.RegisterEvent[[]float64](eventFrame)

	service := newPlayerService()

	app := application.New(application.Options{
		Name:        appName,
		Description: appDescription,
		Services: []application.Service{
			application.NewService(service),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
		Server: application.ServerOptions{
			Host: "127.0.0.1",
			Port: 18080,
		},
	})

	// The effect window is created on demand from the main window (see
	// PlayerService.OpenEffectWindow). It is not created hidden here: a closed
	// window leaves a stale handle, so the service looks it up by name and
	// rebuilds it if it is gone.

	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             mainWindowName,
		Title:            appName,
		Width:            1180,
		Height:           720,
		MinWidth:         820,
		MinHeight:        560,
		BackgroundColour: application.NewRGB(0x1d, 0x1d, 0x20),
		// Files dropped on the window are the other half of "add tracks": a
		// listener folds them into the queue through the same service call the
		// picker uses.
		EnableFileDrop: true,
		URL:            "/",
	})

	// The drop arrives as a window event, not a bound method, so it is wired
	// here where the window exists. The files are handed to the service rather
	// than loaded here, keeping all queue logic in one place.
	window.OnWindowEvent(events.Common.WindowFilesDropped, func(e *application.WindowEvent) {
		files := e.Context().DroppedFiles()
		if len(files) == 0 {
			return
		}
		if err := service.LoadPaths(files); err != nil {
			slog.Error("load dropped files", "error", err)
		}
	})

	if err := app.Run(); err != nil {
		slog.Error("application exited", "error", err)
		os.Exit(1)
	}
}

// openEffectWindow shows the effect window, creating it the first time and
// rebuilding it if it was closed. It is idempotent: a window already open is
// focused rather than duplicated, which is what makes the header's "open in new
// window" safe to click twice.
func (s *PlayerService) openEffectWindow() {
	app := application.Get()
	if app == nil {
		return
	}
	if w, ok := app.Window.GetByName(effectWindowName); ok {
		w.Show()
		w.Focus()

		return
	}

	w := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             effectWindowName,
		Title:            "Effects",
		Width:            880,
		Height:           620,
		MinWidth:         640,
		MinHeight:        420,
		BackgroundColour: application.NewRGB(0x1d, 0x1d, 0x20),
		URL:              "/?window=effects",
	})
	w.Show()
	w.Focus()
}
