package decode

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// flacBenchFile resolves the benchmark fixture. The long file lives outside the
// repo, so a missing one skips rather than fails and an env var can point
// elsewhere.
func flacBenchFile(b *testing.B, env, fallback string) string {
	b.Helper()

	if path := os.Getenv(env); path != "" {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	path := filepath.Join("testdata", fallback)
	if _, err := os.Stat(path); err != nil {
		b.Skipf("no fixture: set %s or provide %s", env, path)
	}

	return path
}

func BenchmarkFlacDecode(b *testing.B) {
	path := flacBenchFile(b, "PLAYER_BENCH_FLAC", "sine_stereo_48k.flac")
	buf := make([]float32, 4800*2)
	b.ResetTimer()
	for b.Loop() {
		d, err := NewFlacFactory().Open(path)
		if err != nil {
			b.Fatal(err)
		}
		for {
			if _, err := d.ReadFrames(buf); err != nil {
				if !errors.Is(err, io.EOF) {
					b.Fatal(err)
				}

				break
			}
		}
		d.Close()
	}
}

func BenchmarkFlacOpen(b *testing.B) {
	path := flacBenchFile(b, "PLAYER_BENCH_FLAC", "sine_stereo_48k.flac")
	b.ResetTimer()
	for b.Loop() {
		d, err := NewFlacFactory().Open(path)
		if err != nil {
			b.Fatal(err)
		}
		d.Close()
	}
}

func BenchmarkFlacSeekOnly(b *testing.B) {
	path := flacBenchFile(b, "PLAYER_BENCH_FLAC", "sine_stereo_48k.flac")
	d, err := NewFlacFactory().Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	seeker := d.(Seeker)
	total := d.Info().TotalFrames
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		target := int64(i*48000) % (total - 48000)
		if err := seeker.SeekFrame(target); err != nil {
			b.Fatalf("SeekFrame: %v", err)
		}
	}
}

func BenchmarkFlacSeekFirst100ms(b *testing.B) {
	path := flacBenchFile(b, "PLAYER_BENCH_FLAC", "sine_stereo_48k.flac")
	d, err := NewFlacFactory().Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	seeker := d.(Seeker)
	total := d.Info().TotalFrames
	buf := make([]float32, 4800*2)
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		target := int64(i*48000) % (total - 48000)
		if err := seeker.SeekFrame(target); err != nil {
			b.Fatalf("SeekFrame: %v", err)
		}
		if _, err := d.ReadFrames(buf); err != nil {
			b.Fatalf("ReadFrames: %v", err)
		}
	}
}
