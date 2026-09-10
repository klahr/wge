package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/klahr/wge/internal/broker"
	"github.com/klahr/wge/internal/store"
)

// A pool must not turn a precise refusal into a generic one.
func TestPoolErrorsStayClassifiable(t *testing.T) {
	d := &Docker{nodes: []*node{{name: "a"}}}
	err := d.everyNode(func(*node) error {
		return fmt.Errorf("probe said 65534: %w", ErrSetuidIgnored)
	})
	if !errors.Is(err, ErrSetuidIgnored) {
		t.Fatalf("errors.Is lost the cause through the pool: %v", err)
	}

	many := &Docker{nodes: []*node{{name: "a"}, {name: "b"}}}
	err = many.everyNode(func(w *node) error {
		if w.name == "b" {
			return fmt.Errorf("%w", ErrStorageQuotaIgnored)
		}
		return nil
	})
	if !errors.Is(err, ErrStorageQuotaIgnored) {
		t.Fatalf("a failure on one node of several was not classifiable: %v", err)
	}
	if got := err.Error(); got == "" || got[:5] != "node " {
		t.Errorf("multi-node failure should name the node, got %q", got)
	}
}

// A placed run goes back where it was: its scratch space is there and nowhere
// else, and that is the one thing about a run that cannot be rebuilt.
func TestPlacedRunsReturnToTheirMachine(t *testing.T) {
	d := poolOf("a", "b", "c")

	for _, want := range []string{"a", "b", "c"} {
		got, err := d.place(context.Background(), sessionOn(want))
		if err != nil {
			t.Fatalf("place on %s: %v", want, err)
		}
		if got.name != want {
			t.Errorf("run placed on %s went to %s", want, got.name)
		}
	}
}

// A machine that has left the pool takes its containers and its scratch with
// it. The game survives -- it is a function of the salt -- but the placement
// has to be made again.
func TestRunsOnAMissingMachineAreRescheduled(t *testing.T) {
	d := poolOf("a", "b")
	d.byName["a"].reaper.hold(target{Name: ContainerName(1, "main"), RunID: 1})

	got, err := d.place(context.Background(), sessionOn("gone"))
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if got.name != "b" {
		t.Fatalf("rescheduled onto %q, want the emptiest machine", got.name)
	}
}

// Spread rather than packed: losing a machine should take as few players with
// it as possible.
func TestNewRunsGoToTheEmptiestMachine(t *testing.T) {
	d := poolOf("a", "b")
	ctx := context.Background()

	// Fill a up a bit.
	for i := 1; i <= 3; i++ {
		d.byName["a"].reaper.hold(target{Name: ContainerName(int64(i), "main"), RunID: int64(i)})
	}

	got, err := d.place(ctx, sessionOn(""))
	if err != nil {
		t.Fatal(err)
	}
	if got.name != "b" {
		t.Fatalf("a new run went to %s, the busier machine", got.name)
	}
}

// One machine is the ordinary case and must not pay for the general one.
func TestSingleMachinePoolPlacesWithoutAsking(t *testing.T) {
	d := poolOf("only")
	got, err := d.place(context.Background(), sessionOn("somewhere-else"))
	if err != nil {
		t.Fatal(err)
	}
	if got.name != "only" {
		t.Fatalf("placed on %s", got.name)
	}
}

// Two machines under one name means one of them silently serves the other's
// runs, which is a scratch volume that is not where the player left it.
func TestDuplicateNodeNamesAreRefused(t *testing.T) {
	_, err := NewDocker(Options{
		Images: stubImages{},
		Seeder: &stubSeeder{},
		Logger: discardLogger(),
		Nodes: []Node{
			{Name: "a", Socket: "tcp://one:2375"},
			{Name: "a", Socket: "tcp://two:2375"},
		},
	})
	if err == nil {
		t.Fatal("a pool with two machines called a was accepted")
	}
}

// A machine with no name cannot be recorded as a run's placement.
func TestNamelessNodesAreRefused(t *testing.T) {
	_, err := NewDocker(Options{
		Images: stubImages{},
		Seeder: &stubSeeder{},
		Logger: discardLogger(),
		Nodes:  []Node{{Socket: "tcp://one:2375"}},
	})
	if err == nil {
		t.Fatal("a pool with a nameless machine was accepted")
	}
}

// -node names this machine, -nodes names a pool. Together, one of them is
// ignored, and an operator cannot tell which.
func TestNodeAndNodesTogetherAreRefused(t *testing.T) {
	_, err := NewDocker(Options{
		Images: stubImages{},
		Seeder: &stubSeeder{},
		Logger: discardLogger(),
		Node:   "this-one",
		Nodes:  []Node{{Name: "a", Socket: "tcp://one:2375"}},
	})
	if err == nil {
		t.Fatal("both were accepted")
	}
}

func poolOf(names ...string) *Docker {
	d := &Docker{log: discardLogger(), byName: map[string]*node{}}
	for _, name := range names {
		limits := DefaultLimits()
		limits.MinFreeBytes = 0

		n := &node{name: name, limits: limits, log: discardLogger(), maxMachines: 10}
		n.reaper = newReaper(DefaultGrace, func(target) {})
		d.nodes = append(d.nodes, n)
		d.byName[name] = n
	}
	return d
}

func sessionOn(host string) *broker.Session {
	return &broker.Session{Run: &store.Run{ID: 1, CurrentHost: host}}
}
