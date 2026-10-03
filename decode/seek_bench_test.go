package decode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pion/opus"
)

// benchOpusFile resolves a benchmark fixture. The long fixtures live outside the
// repo (they are generated, not checked in), so a missing file skips rather than
// fails and an env var can point at another band. The checked-in fixtures are
// always used as a fallback so the benchmarks run anywhere.
func benchOpusFile(b *testing.B, env, fallback string) string {
	b.Helper()

	if path := os.Getenv(env); path != "" {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	path := filepath.Join("testdata", fallback)
	if _, err := os.Stat(path); err != nil {
		b.Skipf("no benchmark fixture: set %s or provide %s", env, path)
	}

	return path
}

// seekTargets are spread across a long fixture so the seek cost cannot be an
// artifact of one page.
func seekTargets(total int64) []int64 {
	if total <= 0 {
		total = 180 * 48000
	}

	return []int64{total / 4, total / 2, total * 3 / 4}
}

// BenchmarkPionSeekFrame times SeekFrame alone for the fast pure-Go variant,
// the automatic default.
func BenchmarkPionSeekFrame(b *testing.B) {
	path := benchOpusFile(b, "PLAYER_BENCH_OPUS", "stereo_2s.opus")

	d, err := NewPionOpusFactory().Open(path)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer d.Close()
	seeker := d.(Seeker)
	targets := seekTargets(d.Info().TotalFrames)

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if err := seeker.SeekFrame(targets[i%len(targets)]); err != nil {
			b.Fatalf("SeekFrame: %v", err)
		}
	}
}

// BenchmarkPionExactSeekFrame is the same measurement for the bit-perfect
// variant, whose 800 ms warm-up is the cost of byte identity after a seek.
func BenchmarkPionExactSeekFrame(b *testing.B) {
	path := benchOpusFile(b, "PLAYER_BENCH_OPUS", "stereo_2s.opus")

	d, err := NewPionOpusExactFactory().Open(path)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer d.Close()
	seeker := d.(Seeker)
	targets := seekTargets(d.Info().TotalFrames)

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if err := seeker.SeekFrame(targets[i%len(targets)]); err != nil {
			b.Fatalf("SeekFrame: %v", err)
		}
	}
}

// BenchmarkPionSeekFirst100ms times the fair "time to first correct sample"
// for the fast variant: a seek followed by reading 100 ms. Decoding the
// warm-up is paid here, so this is the number that reflects the work the skip
// removes.
func BenchmarkPionSeekFirst100ms(b *testing.B) {
	path := benchOpusFile(b, "PLAYER_BENCH_OPUS", "stereo_2s.opus")

	d, err := NewPionOpusFactory().Open(path)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer d.Close()
	seeker := d.(Seeker)
	targets := seekTargets(d.Info().TotalFrames)
	buf := make([]float32, 4800*2)

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if err := seeker.SeekFrame(targets[i%len(targets)]); err != nil {
			b.Fatalf("SeekFrame: %v", err)
		}
		if _, err := d.ReadFrames(buf); err != nil {
			b.Fatalf("ReadFrames: %v", err)
		}
	}
}

// BenchmarkPionExactSeekFirst100ms is the exact variant's fair comparison.
func BenchmarkPionExactSeekFirst100ms(b *testing.B) {
	path := benchOpusFile(b, "PLAYER_BENCH_OPUS", "stereo_2s.opus")

	d, err := NewPionOpusExactFactory().Open(path)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer d.Close()
	seeker := d.(Seeker)
	targets := seekTargets(d.Info().TotalFrames)
	buf := make([]float32, 4800*2)

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if err := seeker.SeekFrame(targets[i%len(targets)]); err != nil {
			b.Fatalf("SeekFrame: %v", err)
		}
		if _, err := d.ReadFrames(buf); err != nil {
			b.Fatalf("ReadFrames: %v", err)
		}
	}
}

// BenchmarkPionSeekFramePages100ms runs the same seek against a small-page
// fixture so a before/after comparison can show the cost no longer tracks the
// page span.
func BenchmarkPionSeekFramePages100ms(b *testing.B) {
	path := benchOpusFile(b, "PLAYER_BENCH_OPUS_PAGES", "stereo_2s.opus")

	d, err := NewPionOpusFactory().Open(path)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer d.Close()
	seeker := d.(Seeker)
	targets := seekTargets(d.Info().TotalFrames)

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if err := seeker.SeekFrame(targets[i%len(targets)]); err != nil {
			b.Fatalf("SeekFrame: %v", err)
		}
	}
}

// BenchmarkPionDecodePacket times a single 20 ms packet so the decoded-packet
// budget converts to time.
func BenchmarkPionDecodePacket(b *testing.B) {
	path := benchOpusFile(b, "PLAYER_BENCH_OPUS", "stereo_2s.opus")

	f, err := os.Open(path)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer f.Close()
	r, err := newOggOpusReader(f)
	if err != nil {
		b.Fatalf("newOggOpusReader: %v", err)
	}
	pkt, _, err := r.ReadPacket()
	if err != nil {
		b.Fatalf("ReadPacket: %v", err)
	}
	dec, err := opus.NewDecoderWithOutput(48000, 2)
	if err != nil {
		b.Fatalf("decoder: %v", err)
	}
	buf := make([]float32, maxOpusPacketSamples)

	b.ResetTimer()
	for b.Loop() {
		if _, err := dec.DecodeToFloat32(pkt, buf); err != nil {
			b.Fatalf("DecodeToFloat32: %v", err)
		}
	}
}

// BenchmarkLibopusfileSeekFrame times the native decoder's seek for comparison.
// It skips when the shared library is unavailable.
func BenchmarkLibopusfileSeekFrame(b *testing.B) {
	if loadLibopusfile() != nil {
		b.Skip("libopusfile is not available")
	}
	path := benchOpusFile(b, "PLAYER_BENCH_OPUS", "stereo_2s.opus")

	d, err := NewLibopusfileFactory().Open(path)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer d.Close()
	seeker := d.(Seeker)
	targets := seekTargets(d.Info().TotalFrames)

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if err := seeker.SeekFrame(targets[i%len(targets)]); err != nil {
			b.Fatalf("SeekFrame: %v", err)
		}
	}
}

// BenchmarkLibopusfileSeekFirst100ms is the native half of the fair comparison.
func BenchmarkLibopusfileSeekFirst100ms(b *testing.B) {
	if loadLibopusfile() != nil {
		b.Skip("libopusfile is not available")
	}
	path := benchOpusFile(b, "PLAYER_BENCH_OPUS", "stereo_2s.opus")

	d, err := NewLibopusfileFactory().Open(path)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer d.Close()
	seeker := d.(Seeker)
	targets := seekTargets(d.Info().TotalFrames)
	buf := make([]float32, 4800*2)

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if err := seeker.SeekFrame(targets[i%len(targets)]); err != nil {
			b.Fatalf("SeekFrame: %v", err)
		}
		if _, err := d.ReadFrames(buf); err != nil {
			b.Fatalf("ReadFrames: %v", err)
		}
	}
}
