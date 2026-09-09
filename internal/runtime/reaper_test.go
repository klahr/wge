package runtime

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

// grace used throughout: long enough not to fire while a test is setting up,
// short enough that the suite stays fast.
const testGrace = 60 * time.Millisecond

// collector records what the reaper destroyed.
type collector struct {
	mu    sync.Mutex
	got   []target
	fired chan target
}

func newCollector() *collector {
	return &collector{fired: make(chan target, 8)}
}

func (c *collector) destroy(t target) {
	c.mu.Lock()
	c.got = append(c.got, t)
	c.mu.Unlock()
	c.fired <- t
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.got)
}

// awaitDestroy waits for one destruction, or fails.
func (c *collector) awaitDestroy(t *testing.T) target {
	t.Helper()
	select {
	case got := <-c.fired:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("expected a container to be destroyed")
		return target{}
	}
}

// refuseDestroy fails if anything is destroyed within the window.
func (c *collector) refuseDestroy(t *testing.T, window time.Duration) {
	t.Helper()
	select {
	case got := <-c.fired:
		t.Fatalf("container %s was destroyed when it should not have been", got.Name)
	case <-time.After(window):
	}
}

func TestContainerIsDestroyedAfterItsLastSession(t *testing.T) {
	c := newCollector()
	r := newReaper(testGrace, c.destroy)
	spot := target{Name: "wge-run-1-main", RunID: 1}

	r.hold(spot)
	r.release(spot)

	if got := c.awaitDestroy(t); got != spot {
		t.Fatalf("destroyed %+v, want %+v", got, spot)
	}
}

// A dropped connection, a closed lid, a train tunnel. Reconnecting to find the
// box rebuilt is jarring and costs the player their scrollback for no reason.
func TestReconnectingWithinGraceCancelsDestruction(t *testing.T) {
	c := newCollector()
	r := newReaper(testGrace, c.destroy)
	spot := target{Name: "wge-run-1-main", RunID: 1}

	r.hold(spot)
	r.release(spot)

	// The player comes back before the grace period is out.
	time.Sleep(testGrace / 3)
	r.hold(spot)

	c.refuseDestroy(t, 3*testGrace)

	// And it is still destroyed once they really do leave.
	r.release(spot)
	if got := c.awaitDestroy(t); got != spot {
		t.Fatalf("destroyed %+v, want %+v", got, spot)
	}
}

// A player with two terminals open is still a player.
func TestContainerSurvivesWhileAnySessionHoldsIt(t *testing.T) {
	c := newCollector()
	r := newReaper(testGrace, c.destroy)
	spot := target{Name: "wge-run-1-main", RunID: 1}

	r.hold(spot)
	r.hold(spot)
	r.release(spot)

	c.refuseDestroy(t, 3*testGrace)

	r.release(spot)
	c.awaitDestroy(t)
}

// The sweep must not race the grace timer: a container waiting to be destroyed
// is already spoken for, and collecting it early takes it from a player who is
// inside their grace period.
func TestPendingContainersCountAsHeld(t *testing.T) {
	c := newCollector()
	r := newReaper(time.Hour, c.destroy)
	spot := target{Name: "wge-run-1-main", RunID: 1}

	r.hold(spot)
	if !r.heldNames()[spot.Name] {
		t.Fatal("a container with a live session is not held")
	}

	r.release(spot)
	if !r.heldNames()[spot.Name] {
		t.Fatal("a container awaiting destruction is not held; the sweep would take it early")
	}

	r.forget(spot.Name)
	if r.heldNames()[spot.Name] {
		t.Fatal("a forgotten container is still held")
	}
}

// Containers outlive the process; the sweep on the next start collects them.
func TestStopCancelsPendingDestruction(t *testing.T) {
	c := newCollector()
	r := newReaper(testGrace, c.destroy)
	spot := target{Name: "wge-run-1-main", RunID: 1}

	r.hold(spot)
	r.release(spot)
	r.stop()

	c.refuseDestroy(t, 3*testGrace)

	// And a release after stopping schedules nothing either.
	r.hold(spot)
	r.release(spot)
	c.refuseDestroy(t, 3*testGrace)
}

func TestRunsAreTrackedIndependently(t *testing.T) {
	c := newCollector()
	r := newReaper(testGrace, c.destroy)

	first := target{Name: "wge-run-1-main", RunID: 1}
	second := target{Name: "wge-run-2-main", RunID: 2}

	r.hold(first)
	r.hold(second)
	r.release(first)

	if got := c.awaitDestroy(t); got != first {
		t.Fatalf("destroyed %+v, want the released one %+v", got, first)
	}
	c.refuseDestroy(t, 2*testGrace)

	r.release(second)
	if got := c.awaitDestroy(t); got != second {
		t.Fatalf("destroyed %+v, want %+v", got, second)
	}
}

// Sessions come and go concurrently: several terminals against one run, and
// several runs at once. The race detector does the asserting here; what is
// checked afterwards is that the bookkeeping balances.
func TestConcurrentHoldsAndReleasesAreSafe(t *testing.T) {
	c := newCollector()
	r := newReaper(testGrace, c.destroy)

	const runs, sessions = 8, 25
	var wg sync.WaitGroup

	for run := 0; run < runs; run++ {
		spot := target{Name: fmt.Sprintf("wge-run-%d-main", run), RunID: int64(run)}
		for session := 0; session < sessions; session++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.hold(spot)
				time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond)
				r.release(spot)
			}()
		}
	}
	wg.Wait()

	// Every container should now be counted down to nothing and waiting on its
	// grace period, not still held by a session that never let go.
	held := r.heldNames()
	if len(held) != runs {
		t.Fatalf("%d containers awaiting destruction, want %d", len(held), runs)
	}

	r.mu.Lock()
	leaked := len(r.holds)
	r.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("%d containers still claimed after every session released", leaked)
	}

	// And each is destroyed exactly once.
	deadline := time.After(3 * time.Second)
	seen := map[string]int{}
	for len(seen) < runs {
		select {
		case got := <-c.fired:
			seen[got.Name]++
			if seen[got.Name] > 1 {
				t.Fatalf("%s destroyed %d times", got.Name, seen[got.Name])
			}
		case <-deadline:
			t.Fatalf("only %d of %d containers were destroyed", len(seen), runs)
		}
	}
}

func TestDefaultGraceIsUsedWhenUnset(t *testing.T) {
	r := newReaper(0, func(target) {})
	if r.grace != DefaultGrace {
		t.Fatalf("grace = %v, want %v", r.grace, DefaultGrace)
	}
}
