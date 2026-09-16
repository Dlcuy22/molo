package decode

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// fixturePath resolves a file in testdata, failing the test when it is missing
// so a forgotten generate.sh run is loud instead of silently skipped.
func fixturePath(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join("testdata", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture %s is missing; run testdata/generate.sh: %v", name, err)
	}

	return path
}

func abs64(v float64) float64 {
	if v < 0 {
		return -v
	}

	return v
}

func float32bits(f float32) uint32 {
	return math.Float32bits(f)
}
