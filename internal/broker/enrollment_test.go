package broker

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestLimiterAllowsUpToTheLimit(t *testing.T) {
	l := newEnrollLimiter(3, time.Hour)
	now := time.Now()

	for i := 0; i < 3; i++ {
		if !l.allow("10.0.0.1:22", now) {
			t.Fatalf("attempt %d refused inside the limit", i+1)
		}
	}
	if l.allow("10.0.0.1:22", now) {
		t.Fatal("the fourth attempt was allowed past a limit of three")
	}
}

// One address is one client, however many connections it opens.
func TestLimiterIgnoresThePort(t *testing.T) {
	l := newEnrollLimiter(2, time.Hour)
	now := time.Now()

	l.allow("10.0.0.1:40001", now)
	l.allow("10.0.0.1:40002", now)

	if l.allow("10.0.0.1:40003", now) {
		t.Fatal("a new source port bought another attempt")
	}
}

// A workshop behind one address must not lock out the office next door.
func TestLimiterKeepsAddressesApart(t *testing.T) {
	l := newEnrollLimiter(1, time.Hour)
	now := time.Now()

	if !l.allow("10.0.0.1:22", now) || !l.allow("10.0.0.2:22", now) {
		t.Fatal("two addresses interfered with each other")
	}
	if l.allow("10.0.0.1:22", now) {
		t.Fatal("the first address got a second attempt")
	}
}

func TestLimiterForgetsOldAttempts(t *testing.T) {
	l := newEnrollLimiter(2, time.Hour)
	start := time.Now()

	l.allow("10.0.0.1:22", start)
	l.allow("10.0.0.1:22", start)
	if l.allow("10.0.0.1:22", start) {
		t.Fatal("allowed past the limit")
	}

	// An hour and a minute later, the window has moved on.
	later := start.Add(time.Hour + time.Minute)
	if !l.allow("10.0.0.1:22", later) {
		t.Fatal("attempts from an hour ago still count")
	}
}

func TestLimiterCanBeTurnedOff(t *testing.T) {
	l := newEnrollLimiter(-1, time.Hour)
	now := time.Now()

	for i := 0; i < 500; i++ {
		if !l.allow("10.0.0.1:22", now) {
			t.Fatalf("attempt %d refused with the limit off", i)
		}
	}
}

// The map is keyed by whoever has ever connected, and nothing else prunes it.
func TestLimiterDoesNotGrowWithoutBound(t *testing.T) {
	l := newEnrollLimiter(1, time.Minute)
	start := time.Now()

	for i := 0; i < 5000; i++ {
		l.allow(fmt.Sprintf("10.%d.%d.%d:22", i/65536, (i/256)%256, i%256), start)
	}

	// Long enough later that everything above has fallen out of the window.
	l.allow("192.0.2.1:22", start.Add(2*time.Minute))

	l.mu.Lock()
	size := len(l.seen)
	l.mu.Unlock()
	if size > 4096 {
		t.Fatalf("the limiter is holding %d addresses", size)
	}
}

func TestLimiterIsSafeUnderConcurrency(t *testing.T) {
	l := newEnrollLimiter(50, time.Hour)
	now := time.Now()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var allowed int

	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.allow("10.0.0.1:22", now) {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowed != 50 {
		t.Fatalf("%d attempts allowed through a limit of 50", allowed)
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	l := newEnrollLimiter(0, 0)
	if l.limit != DefaultEnrollLimit || l.window != DefaultEnrollWindow {
		t.Fatalf("limiter = %d per %v, want the defaults", l.limit, l.window)
	}
}
