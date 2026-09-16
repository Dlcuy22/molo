package stream

import (
	"testing"
	"time"
)

// waitWithin fails instead of hanging when a wakeup never arrives, so a missing
// broadcast shows up as a test failure rather than a stuck suite.
func waitWithin(t *testing.T, timeout time.Duration, what string, done <-chan struct{}) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("timed out after %v waiting for %s", timeout, what)
	}
}

func TestGateWakesReaderOnWrite(t *testing.T) {
	g := newGate()
	var items []int

	woke := make(chan int, 1)
	go func() {
		g.mu.Lock()
		ok := g.await(func() bool { return len(items) > 0 })
		if !ok {
			g.mu.Unlock()

			return
		}
		v := items[0]
		items = items[1:]
		g.mu.Unlock()
		woke <- v
	}()

	// Let the reader park before publishing. A polling loop would busy-wait
	// through this sleep; the cond must simply be asleep.
	time.Sleep(20 * time.Millisecond)

	g.mu.Lock()
	items = append(items, 42)
	g.mu.Unlock()
	g.wake()

	select {
	case got := <-woke:
		if got != 42 {
			t.Fatalf("reader saw %d, want 42", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader stayed parked after a write and wake")
	}
}

func TestGateWakesWriterOnRead(t *testing.T) {
	g := newGate()
	full := true

	proceeded := make(chan struct{})
	go func() {
		g.mu.Lock()
		ok := g.await(func() bool { return !full })
		g.mu.Unlock()
		if ok {
			close(proceeded)
		}
	}()

	time.Sleep(20 * time.Millisecond)

	g.mu.Lock()
	full = false
	g.mu.Unlock()
	g.wake()

	waitWithin(t, 2*time.Second, "writer wake", proceeded)
}

func TestGateCloseWakesEveryoneWithoutDeadlock(t *testing.T) {
	g := newGate()

	readerDone := make(chan struct{})
	writerDone := make(chan struct{})
	// Both sides park on predicates that never become true, so only Close can
	// release them. Without the broadcast this test hangs.
	go func() {
		g.mu.Lock()
		g.await(func() bool { return false })
		g.mu.Unlock()
		close(readerDone)
	}()
	go func() {
		g.mu.Lock()
		g.await(func() bool { return false })
		g.mu.Unlock()
		close(writerDone)
	}()

	time.Sleep(20 * time.Millisecond)
	g.close()

	waitWithin(t, 2*time.Second, "reader after close", readerDone)
	waitWithin(t, 2*time.Second, "writer after close", writerDone)
}

func TestGateCloseIsIdempotent(t *testing.T) {
	g := newGate()
	g.close()
	g.close()

	g.mu.Lock()
	ok := g.await(func() bool { return false })
	g.mu.Unlock()
	if ok {
		t.Fatal("await on a closed gate reported ready")
	}
}

func TestGateAwaitReturnsImmediatelyWhenAlreadyReady(t *testing.T) {
	g := newGate()

	done := make(chan struct{})
	go func() {
		g.mu.Lock()
		ok := g.await(func() bool { return true })
		g.mu.Unlock()
		if !ok {
			t.Error("await reported closed for an open gate")
		}
		close(done)
	}()

	waitWithin(t, 2*time.Second, "immediate await", done)
}
