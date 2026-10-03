package stream

import "sync"

// gate parks goroutines on a condition while they wait for the ring to change
// state. It exists so the producer and the consumer sleep instead of polling;
// a spin loop of runtime.Gosched plus a one millisecond sleep burns a core and
// adds latency to every wakeup.
//
// One condition and one broadcast wake everybody: producers and consumers
// re-check their predicates, which is cheap next to the cost of getting a
// wakeup wrong. The mutex guards the gate's own state and, in the streamer,
// the ring's read side; it is never held while waiting for data to arrive.
type gate struct {
	mu     sync.Mutex
	cond   *sync.Cond
	closed bool
}

func newGate() *gate {
	g := &gate{}
	g.cond = sync.NewCond(&g.mu)

	return g
}

// await parks the caller until ready reports true or the gate closes. The
// caller must hold the mutex, which await releases while parked. It returns
// false when the gate is closed, so callers can tell a real wakeup from a
// shutdown without inspecting the flag themselves.
func (g *gate) await(ready func() bool) bool {
	for !g.closed && !ready() {
		g.cond.Wait()
	}

	return !g.closed
}

// wake broadcasts a state change. It takes the mutex so a wakeup cannot slip
// between a waiter's predicate check and its park, which is the classic lost
// wakeup that turns a cond into a hang.
func (g *gate) wake() {
	g.mu.Lock()
	g.cond.Broadcast()
	g.mu.Unlock()
}

// close releases every parked goroutine for good. Await calls then report
// false, so blocked producers and consumers return promptly instead of leaking.
func (g *gate) close() {
	g.mu.Lock()
	g.closed = true
	g.cond.Broadcast()
	g.mu.Unlock()
}
