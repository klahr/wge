package runtime

import (
	"sync"
	"time"
)

// DefaultGrace is how long a run's container outlives its last session.
//
// Long enough to survive a dropped connection, a closed laptop lid or a train
// tunnel -- reconnecting to find the box rebuilt is jarring, and a player who
// was mid-thought loses their scrollback for no reason. Short enough that an
// abandoned game is not still holding memory an hour later.
const DefaultGrace = 15 * time.Minute

// DefaultSweep is how often the engine looks for containers nothing is holding.
const DefaultSweep = 5 * time.Minute

// target names a container and the run it belongs to.
type target struct {
	Name  string
	RunID int64
}

// reaper destroys a container once nothing is using it.
//
// It is a reference count with a delay. Sessions hold a container; when the
// last one lets go, destruction is scheduled, and a session arriving inside the
// grace period cancels it. This is only safe because a container is a pure
// function of (game_version, run_salt): reaping one costs a player their
// scrollback and nothing else, and the next connection rebuilds it.
type reaper struct {
	grace   time.Duration
	destroy func(target)

	mu      sync.Mutex
	holds   map[string]int
	pending map[string]*time.Timer
	stopped bool
}

func newReaper(grace time.Duration, destroy func(target)) *reaper {
	if grace <= 0 {
		grace = DefaultGrace
	}
	return &reaper{
		grace:   grace,
		destroy: destroy,
		holds:   map[string]int{},
		pending: map[string]*time.Timer{},
	}
}

// hold claims a container for a session, cancelling any scheduled destruction.
//
// It must be called before the container is created, not after: otherwise a
// sweep running in the gap sees a container nothing is holding and destroys it
// while the session that asked for it is still starting up.
func (r *reaper) hold(t target) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if timer, ok := r.pending[t.Name]; ok {
		timer.Stop()
		delete(r.pending, t.Name)
	}
	r.holds[t.Name]++
}

// release gives up a session's claim. When the last one goes, destruction is
// scheduled for the end of the grace period.
func (r *reaper) release(t target) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.holds[t.Name]--
	if r.holds[t.Name] > 0 {
		return
	}
	delete(r.holds, t.Name)

	if r.stopped {
		return
	}
	r.pending[t.Name] = time.AfterFunc(r.grace, func() {
		r.mu.Lock()
		// A session may have arrived between the timer firing and this lock.
		if r.holds[t.Name] > 0 {
			r.mu.Unlock()
			return
		}
		delete(r.pending, t.Name)
		r.mu.Unlock()

		r.destroy(t)
	})
}

// heldNames returns the containers currently claimed or awaiting destruction.
//
// A container awaiting destruction still counts as spoken for: the sweep must
// not race the grace timer and take it early.
func (r *reaper) heldNames() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	names := make(map[string]bool, len(r.holds)+len(r.pending))
	for name := range r.holds {
		names[name] = true
	}
	for name := range r.pending {
		names[name] = true
	}
	return names
}

// forget drops all state for a container, after it has been destroyed by
// something other than its own timer.
func (r *reaper) forget(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if timer, ok := r.pending[name]; ok {
		timer.Stop()
		delete(r.pending, name)
	}
	delete(r.holds, name)
}

// stop cancels every pending destruction. Containers outlive the process; the
// sweep on the next start is what collects them.
func (r *reaper) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.stopped = true
	for name, timer := range r.pending {
		timer.Stop()
		delete(r.pending, name)
	}
}
