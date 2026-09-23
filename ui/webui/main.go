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

	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
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
