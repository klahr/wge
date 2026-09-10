package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/klahr/wge/internal/broker"
)

// admissionFor builds a runtime with a fixed capacity and no engine behind it.
//
// The disk floor is off: these tests are about counting machines, and leaving
// it on would have them ask a Docker engine that is not there.
func admissionFor(capacity int) *node {
	limits := DefaultLimits()
	limits.MinFreeBytes = 0

	d := &node{
		limits:      limits,
		log:         slog.New(slog.DiscardHandler),
		maxMachines: capacity,
	}
	d.reaper = newReaper(DefaultGrace, func(target) {})
	return d
}

func machines(runID int64, hosts ...string) []target {
	out := make([]target, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, target{Name: ContainerName(runID, h), RunID: runID})
	}
	return out
}

func TestAdmissionRefusesWhenFull(t *testing.T) {
	ctx := context.Background()
	d := admissionFor(2)

	if err := d.admit(ctx, machines(1, "main")); err != nil {
		t.Fatalf("first run refused: %v", err)
	}
	if err := d.admit(ctx, machines(2, "main")); err != nil {
		t.Fatalf("second run refused: %v", err)
	}

	err := d.admit(ctx, machines(3, "main"))
	if !errors.Is(err, broker.ErrAtCapacity) {
		t.Fatalf("third run got %v, want ErrAtCapacity", err)
	}
	// The message has to be usable by an operator reading a log.
	if got := err.Error(); got == "" || !contains([]string{got}, got) {
		t.Fatal("empty refusal")
	}
}

// A run that spans two machines costs two, and gets neither if only one fits.
func TestMultiHostRunNeedsEveryMachine(t *testing.T) {
	ctx := context.Background()
	d := admissionFor(3)

	if err := d.admit(ctx, machines(1, "relay", "vault")); err != nil {
		t.Fatalf("two-machine run refused with room for three: %v", err)
	}

	// One slot left, and a two-machine run needs two.
	if err := d.admit(ctx, machines(2, "relay", "vault")); !errors.Is(err, broker.ErrAtCapacity) {
		t.Fatalf("got %v, want ErrAtCapacity", err)
	}
	// And a one-machine run still fits.
	if err := d.admit(ctx, machines(3, "main")); err != nil {
		t.Fatalf("single machine refused with a slot free: %v", err)
	}
}

// Admitting half a run leaves the player on a box whose peer will never come
// up, which is a worse failure than being turned away.
func TestRefusalReservesNothing(t *testing.T) {
	ctx := context.Background()
	d := admissionFor(1)

	if err := d.admit(ctx, machines(1, "main")); err != nil {
		t.Fatal(err)
	}
	_ = d.admit(ctx, machines(2, "relay", "vault"))

	held := d.reaper.heldNames()
	for _, name := range []string{ContainerName(2, "relay"), ContainerName(2, "vault")} {
		if held[name] {
			t.Fatalf("%s was reserved by a refused admission", name)
		}
	}
	if len(held) != 1 {
		t.Fatalf("%d machines held after one admission and one refusal", len(held))
	}
}

// Somebody already resident is already counted, and turning them away would
// free nothing.
func TestResidentPlayersAreNeverRefused(t *testing.T) {
	ctx := context.Background()
	d := admissionFor(1)

	if err := d.admit(ctx, machines(1, "main")); err != nil {
		t.Fatal(err)
	}

	// Their next session, with the host full to the brim.
	if err := d.admit(ctx, machines(1, "main")); err != nil {
		t.Fatalf("a player whose machine is already up was refused: %v", err)
	}
}

// A machine inside its grace period is still running and still costs memory.
func TestMachinesAwaitingReapStillCount(t *testing.T) {
	ctx := context.Background()
	d := admissionFor(1)

	spot := machines(1, "main")
	if err := d.admit(ctx, spot); err != nil {
		t.Fatal(err)
	}
	d.reaper.release(spot[0]) // session over; grace running

	if err := d.admit(ctx, machines(2, "main")); !errors.Is(err, broker.ErrAtCapacity) {
		t.Fatalf("got %v; a machine in its grace period is still running", err)
	}
}

// Two connections arriving together must not both be told there is room for
// one. The race detector checks the bookkeeping; the count checks the policy.
func TestConcurrentAdmissionsCannotOversubscribe(t *testing.T) {
	ctx := context.Background()
	const capacity = 10
	d := admissionFor(capacity)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var admitted int

	for run := 1; run <= 60; run++ {
		wg.Add(1)
		go func(run int64) {
			defer wg.Done()
			if err := d.admit(ctx, machines(run, "main")); err == nil {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}(int64(run))
	}
	wg.Wait()

	if admitted != capacity {
		t.Fatalf("admitted %d runs into %d slots", admitted, capacity)
	}
	if held := len(d.reaper.heldNames()); held != capacity {
		t.Fatalf("%d machines held, want %d", held, capacity)
	}
}

func TestCapacityFromMemory(t *testing.T) {
	const gb = 1 << 30

	for _, tc := range []struct {
		name       string
		memTotal   int64
		perMachine int64
		want       int
	}{
		{"16GB host, 512MB machines", 16 * gb, 512 << 20, 24},
		{"4GB host", 4 * gb, 512 << 20, 6},
		{"a host too small still serves somebody", 512 << 20, 512 << 20, MinCapacity},
		{"unknown memory", 0, 512 << 20, MinCapacity},
		{"unlimited machines", 16 * gb, 0, MinCapacity},
	} {
		if got := capacityFor(tc.memTotal, tc.perMachine); got != tc.want {
			t.Errorf("%s: capacityFor(%d, %d) = %d, want %d",
				tc.name, tc.memTotal, tc.perMachine, got, tc.want)
		}
	}
}

// A configured limit is taken as given; nothing is asked of the engine.
func TestConfiguredCapacityWins(t *testing.T) {
	d := admissionFor(7)
	if got := d.capacityLimit(context.Background()); got != 7 {
		t.Fatalf("capacity = %d, want the configured 7", got)
	}
}
