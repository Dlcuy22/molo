package priority

import (
	"runtime"
	"strings"
	"testing"
)

// TestRaiseReportsPlatform pins the debug line the front ends print: it names
// the OS/arch and a priority state, whatever the platform.
func TestRaiseReportsPlatform(t *testing.T) {
	rep, err := Raise()
	if err != nil {
		t.Fatalf("Raise: %v", err)
	}

	if want := runtime.GOOS + "/" + runtime.GOARCH; rep.Platform != want {
		t.Fatalf("Platform = %q, want %q", rep.Platform, want)
	}
	if rep.Priority == "" {
		t.Fatal("Priority is empty")
	}

	lines := rep.Lines()
	if len(lines) != 2 {
		t.Fatalf("Lines() = %d, want 2", len(lines))
	}
	if !strings.HasPrefix(lines[0], "Detected running on ") {
		t.Fatalf("platform line = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "Priority: ") {
		t.Fatalf("priority line = %q", lines[1])
	}
}

// TestRaiseIsBestEffort pins the contract callers rely on: Raise never panics
// and is safe to call more than once.
func TestRaiseIsBestEffort(t *testing.T) {
	if _, err := Raise(); err != nil {
		t.Fatalf("Raise: %v", err)
	}
	if _, err := Raise(); err != nil {
		t.Fatalf("second Raise: %v", err)
	}
}
