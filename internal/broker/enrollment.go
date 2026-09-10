package broker

import (
	"net"
	"sync"
	"time"
)

// DefaultEnrollLimit is how many times one address may try to enrol in a
// window.
//
// Generous on purpose: a workshop of thirty people behind one office address
// is the normal case, and a limit that turns them away is worse than the abuse
// it prevents. What it stops is the unbounded case -- a script creating players
// until the node is full, or working through invitation codes.
const DefaultEnrollLimit = 60

// DefaultEnrollWindow is the period the limit applies over.
const DefaultEnrollWindow = time.Hour

// enrollLimiter bounds how often one address may attempt to enrol.
//
// It is deliberately in memory and per process. Enrolment attempts are not
// worth a table, and everything else the engine holds in a container is
// discarded on restart anyway.
type enrollLimiter struct {
	limit  int
	window time.Duration

	mu   sync.Mutex
	seen map[string][]time.Time
}

func newEnrollLimiter(limit int, window time.Duration) *enrollLimiter {
	if limit == 0 {
		limit = DefaultEnrollLimit
	}
	if window <= 0 {
		window = DefaultEnrollWindow
	}
	return &enrollLimiter{limit: limit, window: window, seen: map[string][]time.Time{}}
}

// allow records an attempt and reports whether it is within the limit.
func (l *enrollLimiter) allow(addr string, now time.Time) bool {
	if l.limit < 0 {
		return true
	}

	host := hostOf(addr)

	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-l.window)
	kept := l.seen[host][:0]
	for _, at := range l.seen[host] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}

	if len(kept) >= l.limit {
		l.seen[host] = kept
		return false
	}

	l.seen[host] = append(kept, now)

	// Addresses that have gone quiet are dropped rather than accumulated: the
	// map is keyed by whoever has ever connected, and nothing else prunes it.
	if len(l.seen) > 4096 {
		l.prune(cutoff)
	}
	return true
}

func (l *enrollLimiter) prune(cutoff time.Time) {
	for host, attempts := range l.seen {
		kept := attempts[:0]
		for _, at := range attempts {
			if at.After(cutoff) {
				kept = append(kept, at)
			}
		}
		if len(kept) == 0 {
			delete(l.seen, host)
			continue
		}
		l.seen[host] = kept
	}
}

// hostOf drops the port, so that one address is one client however many
// connections it opens.
func hostOf(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
