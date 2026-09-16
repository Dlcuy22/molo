package stream

import (
	"math/rand"
	"testing"
)

func TestNewRingRoundsCapacityToPowerOfTwo(t *testing.T) {
	tests := []struct {
		name   string
		frames int
		ch     int
		want   int64
	}{
		{"already a power of two", 1024, 2, 1024},
		{"rounds up", 1000, 2, 1024},
		{"one frame", 1, 1, 1},
		{"just past a power", 1025, 2, 2048},
		{"huge", 1 << 20, 2, 1 << 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRing(tt.frames, tt.ch)
			if got := r.Cap(); got != tt.want {
				t.Fatalf("Cap() = %d, want %d", got, tt.want)
			}
			if r.Ch() != tt.ch {
				t.Fatalf("Ch() = %d, want %d", r.Ch(), tt.ch)
			}
			if r.Len() != 0 {
				t.Fatalf("new ring Len() = %d, want 0", r.Len())
			}
			if r.Space() != tt.want {
				t.Fatalf("new ring Space() = %d, want %d", r.Space(), tt.want)
			}
		})
	}
}

func TestNewRingRejectsInvalidArguments(t *testing.T) {
	// Capacity is rounded up, never silently clamped to something usable, so a
	// zero or negative request is a programming error rather than a quiet no-op.
	for _, tt := range []struct {
		name   string
		frames int
		ch     int
	}{
		{"zero frames", 0, 2},
		{"negative frames", -4, 2},
		{"zero channels", 16, 0},
		{"negative channels", 16, -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewRing accepted invalid arguments")
				}
			}()
			NewRing(tt.frames, tt.ch)
		})
	}
}

func TestRingWriteReadRoundTrip(t *testing.T) {
	r := NewRing(8, 2)
	write := []float32{1, 2, 3, 4, 5, 6}
	if got := r.Write(write); got != 3 {
		t.Fatalf("Write() = %d frames, want 3", got)
	}
	if got := r.Len(); got != 3 {
		t.Fatalf("Len() = %d, want 3", got)
	}
	if got := r.Space(); got != 5 {
		t.Fatalf("Space() = %d, want 5", got)
	}

	read := make([]float32, 6)
	if got := r.Read(read); got != 3 {
		t.Fatalf("Read() = %d frames, want 3", got)
	}
	for i := range write {
		if read[i] != write[i] {
			t.Fatalf("sample %d = %v, want %v", i, read[i], write[i])
		}
	}
}

func TestRingIsNonBlockingWhenFullOrEmpty(t *testing.T) {
	r := NewRing(2, 1)
	if got := r.Write([]float32{1, 2, 3, 4}); got != 2 {
		t.Fatalf("Write into a smaller ring = %d frames, want 2", got)
	}
	if got := r.Write([]float32{9}); got != 0 {
		t.Fatalf("Write into a full ring = %d frames, want 0", got)
	}
	if got := r.Read(make([]float32, 4)); got != 2 {
		t.Fatalf("Read() = %d frames, want 2", got)
	}
	if got := r.Read(make([]float32, 4)); got != 0 {
		t.Fatalf("Read from an empty ring = %d frames, want 0", got)
	}
}

func TestRingIgnoresPartialTrailingFrame(t *testing.T) {
	r := NewRing(8, 2)
	// One and a half frames: the trailing half frame has no partner channel and
	// must not be handed to the consumer as a frame.
	if got := r.Write([]float32{1, 2, 3}); got != 1 {
		t.Fatalf("Write() = %d frames, want 1", got)
	}
	dst := make([]float32, 4)
	if got := r.Read(dst); got != 1 {
		t.Fatalf("Read() = %d frames, want 1", got)
	}
	if dst[0] != 1 || dst[1] != 2 {
		t.Fatalf("read back %v, want [1 2]", dst[:2])
	}
}

func TestRingWraparoundPreservesOrder(t *testing.T) {
	r := NewRing(4, 2)

	// Walk the logical indices past the physical end several times so the
	// bitmask must wrap on both the write and the read side.
	for base := float32(0); base < 40; base += 3 {
		in := []float32{base, base + 1, base + 2, base + 3, base + 4, base + 5}
		if got := r.Write(in); got != 3 {
			t.Fatalf("Write() = %d frames, want 3", got)
		}
		out := make([]float32, 6)
		if got := r.Read(out); got != 3 {
			t.Fatalf("Read() = %d frames, want 3", got)
		}
		for i := range out {
			if out[i] != in[i] {
				t.Fatalf("after base %v sample %d = %v, want %v", base, i, out[i], in[i])
			}
		}
	}
}

func TestRingResetDropsUnreadFrames(t *testing.T) {
	r := NewRing(8, 2)
	r.Write([]float32{1, 2, 3, 4, 5, 6})
	out := make([]float32, 2)
	r.Read(out)

	r.Reset()
	if r.Len() != 0 {
		t.Fatalf("Len() after Reset = %d, want 0", r.Len())
	}
	if r.Space() != 8 {
		t.Fatalf("Space() after Reset = %d, want 8", r.Space())
	}

	// Writing after a reset must still be readable; the cursors keep advancing
	// and the bitmask handles the wrap.
	r.Write([]float32{7, 8})
	if got := r.Read(make([]float32, 4)); got != 1 {
		t.Fatalf("Read() after Reset = %d frames, want 1", got)
	}
}

func TestRingMatchesReferenceModel(t *testing.T) {
	const (
		capacity = 16
		channels = 2
		ops      = 20000
	)
	r := NewRing(capacity, channels)

	var (
		rng  = rand.New(rand.NewSource(1))
		ref  []float32
		next float32
	)
	for i := 0; i < ops; i++ {
		if rng.Intn(2) == 0 || len(ref) == 0 {
			n := rng.Intn(capacity + 3)
			in := make([]float32, n*channels)
			for j := range in {
				in[j] = next
				next++
			}
			got := r.Write(in)
			want := min(n, capacity-len(ref)/channels)
			if got != want {
				t.Fatalf("op %d: Write() = %d frames, want %d", i, got, want)
			}
			ref = append(ref, in[:got*channels]...)
		} else {
			n := rng.Intn(capacity + 3)
			out := make([]float32, n*channels)
			got := r.Read(out)
			want := min(n, len(ref)/channels)
			if got != want {
				t.Fatalf("op %d: Read() = %d frames, want %d", i, got, want)
			}
			for j := 0; j < got*channels; j++ {
				if out[j] != ref[j] {
					t.Fatalf("op %d: sample %d = %v, want %v", i, j, out[j], ref[j])
				}
			}
			ref = ref[got*channels:]
		}

		if got := r.Len(); got != int64(len(ref)/channels) {
			t.Fatalf("op %d: Len() = %d, want %d", i, got, len(ref)/channels)
		}
		if got := r.Space(); got != int64(capacity-len(ref)/channels) {
			t.Fatalf("op %d: Space() = %d, want %d", i, got, capacity-len(ref)/channels)
		}
	}
}

func TestRingHotPathsDoNotAllocate(t *testing.T) {
	r := NewRing(64, 2)
	in := make([]float32, 32)
	out := make([]float32, 32)
	r.Write(in)

	writes := testing.AllocsPerRun(1000, func() {
		r.Write(in)
	})
	if writes != 0 {
		t.Fatalf("Write allocated %v times per call, want 0", writes)
	}

	reads := testing.AllocsPerRun(1000, func() {
		r.Read(out)
	})
	if reads != 0 {
		t.Fatalf("Read allocated %v times per call, want 0", reads)
	}
}
