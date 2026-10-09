//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// kernel32 holds the console entry points x/sys does not wrap. AttachConsole,
// AllocConsole and GetConsoleWindow are the three the standard handles cannot
// cover on their own.
var kernel32 = windows.NewLazySystemDLL("kernel32.dll")

// attachConsole points os.Stdout and os.Stderr at a real console and reports
// whether one is available. It attaches to the console of the process that
// launched molo, so a run from a terminal or `wails3 dev` prints there. A new
// console is allocated only when MOLO_DEBUG is set, because a double-clicked
// build of a music player must not pop a console window.
func attachConsole() bool {
	switch {
	case hasConsoleWindow():
		// A console-subsystem build (tests, `go run`, server mode) already has
		// its own console, so the streams work and there is nothing to attach.
		return true
	case hasParentConsole():
		reopenStdHandles()

		return true
	case os.Getenv("MOLO_DEBUG") != "" && allocConsole():
		reopenStdHandles()

		return true
	default:
		return false
	}
}

// hasConsoleWindow reports whether the process already has a console.
func hasConsoleWindow() bool {
	r, _, _ := kernel32.NewProc("GetConsoleWindow").Call()

	return r != 0
}

// hasParentConsole attaches to the launcher's console. It fails when there is
// none, which is the double-click case.
func hasParentConsole() bool {
	const attachParentProcess = ^uintptr(0) // ATTACH_PARENT_PROCESS is (DWORD)-1
	r, _, _ := kernel32.NewProc("AttachConsole").Call(attachParentProcess)

	return r != 0
}

// allocConsole opens a console window, the MOLO_DEBUG opt-in.
func allocConsole() bool {
	r, _, _ := kernel32.NewProc("AllocConsole").Call()

	return r != 0
}

// reopenStdHandles gives the standard streams the console's handles. Go
// captured the original handles at startup, so a GUI build's streams are still
// invalid until the files are replaced.
func reopenStdHandles() {
	if out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout = out
		os.Stderr = out
		windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(out.Fd()))
		windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(out.Fd()))
	}
	if in, err := os.OpenFile("CONIN$", os.O_RDONLY, 0); err == nil {
		os.Stdin = in
		windows.SetStdHandle(windows.STD_INPUT_HANDLE, windows.Handle(in.Fd()))
	}
}
