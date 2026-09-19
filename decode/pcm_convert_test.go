package decode

import (
	"math"
	"testing"
)

// TestConverterCarriesPartialFrames is the regression for the bug the WAV
// adapter hit: a codec library may return short reads that stop mid-frame, and
// the converter must carry the fragment rather than drop it. Dropping shifts
// every later sample by a fraction of a frame, which is silent corruption.
func TestConverterCarriesPartialFrames(t *testing.T) {
	const (
		rate     = 48000
		channels = 2
		bytesPS  = 2
	)
	frameBytes := channels * bytesPS

	// One second of a known ramp, split into awkward chunks that never align to
	// a frame boundary.
	total := make([]byte, 12000*frameBytes)
	for i := range total {
		total[i] = byte(i)
	}

	feedAll := func(chunk int) []float32 {
		c := newPCMConverter(rate, channels, bytesPS)
		for off := 0; off < len(total); off += chunk {
			end := min(off+chunk, len(total))
			c.feed(total[off:end])
		}
		c.markEOF()

		out := make([]float32, 0, 12000*2)
		block := make([]float32, 100*2)
		for {
			n := c.resampleInto(block)
			if n == 0 {
				break
			}
			out = append(out, block[:n*2]...)
		}

		return out
	}

	// A frame-aligned chunk is the reference; the awkward sizes must agree
	// byte for byte, because the same samples were ingested either way.
	want := feedAll(frameBytes * 97)
	for _, chunk := range []int{1, 3, 7, 1000, 4097, 65537} {
		got := feedAll(chunk)
		if len(got) != len(want) {
			t.Fatalf("chunk %d produced %d samples, want %d", chunk, len(got), len(want))
		}
		for i := range got {
			if math.Float32bits(got[i]) != math.Float32bits(want[i]) {
				t.Fatalf("chunk %d sample %d = %v, want %v (a partial frame was dropped)",
					chunk, i, got[i], want[i])
			}
		}
	}
}

// TestConverterFeedFloat covers the float path go-mp3 uses: values pass through
// unchanged and mono is duplicated, exactly as the integer path does.
func TestConverterFeedFloat(t *testing.T) {
	for _, channels := range []int{1, 2} {
		c := newPCMConverter(48000, channels, 4)

		in := []float32{0.25, -0.5}
		raw := make([]byte, 0, len(in)*4)
		for _, v := range in {
			b := math.Float32bits(v)
			raw = append(raw, byte(b), byte(b>>8), byte(b>>16), byte(b>>24))
		}
		c.feedFloat(raw)

		if channels == 1 {
			want := []float32{0.25, 0.25, -0.5, -0.5}
			if len(c.src) != len(want) {
				t.Fatalf("mono float ingest gave %d samples, want %d", len(c.src), len(want))
			}
			for i, w := range want {
				if c.src[i] != w {
					t.Fatalf("mono sample %d = %v, want %v", i, c.src[i], w)
				}
			}

			continue
		}
		if len(c.src) != 2 || c.src[0] != 0.25 || c.src[1] != -0.5 {
			t.Fatalf("stereo float ingest = %v, want [0.25 -0.5]", c.src)
		}
	}
}

// TestConverterSkipFrameBytes pins the priming trim M4A uses: it consumes an
// arbitrary byte count from the front of a chunk, across chunk boundaries.
func TestConverterSkipFrameBytes(t *testing.T) {
	c := newPCMConverter(48000, 2, 2)
	c.feed([]byte{1, 2, 3, 4, 5, 6, 7, 8})

	rest, dropped := c.skipFrameBytes([]byte{9, 10, 11, 12}, 2)
	if dropped != 2 || len(rest) != 2 || rest[0] != 11 || rest[1] != 12 {
		t.Fatalf("skip = (%v, %d), want ([11 12], 2)", rest, dropped)
	}

	// A skip larger than the chunk consumes the whole chunk.
	rest, dropped = c.skipFrameBytes([]byte{1, 2}, 99)
	if dropped != 2 || rest != nil {
		t.Fatalf("oversized skip = (%v, %d), want (nil, 2)", rest, dropped)
	}

	// A zero or negative skip changes nothing.
	raw := []byte{1, 2}
	rest, dropped = c.skipFrameBytes(raw, 0)
	if dropped != 0 || len(rest) != 2 {
		t.Fatalf("zero skip = (%v, %d), want the chunk unchanged", rest, dropped)
	}
}
