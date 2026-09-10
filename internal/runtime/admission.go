package runtime

import (
	"context"
	"fmt"

	"github.com/klahr/wge/internal/broker"
)

// MinCapacity is the smallest number of machines a node will admit. A host too
// small for this is too small to serve anybody.
const MinCapacity = 2

// memoryHeadroom is the share of the engine's memory the games may use.
//
// The rest is for the engine itself, the daemon, and the page cache the
// containers are all reading their image through. Filling a host to its last
// byte does not serve more players; it serves everybody badly and then the
// kernel starts choosing who to kill.
const memoryHeadroom = 0.75

// admit reserves room for a run's machines, or refuses.
//
// Admission is by machine rather than by player, because a run that spans two
// hosts costs twice as much to serve -- and it is all-or-nothing, because
// admitting half of one leaves the player on a box whose peer will never come
// up, which is a worse failure than being turned away.
//
// A player whose machines are already up is always admitted. They are already
// resident and already counted, and turning them away would free nothing.
func (d *Docker) admit(ctx context.Context, targets []target) error {
	d.admitMu.Lock()
	defer d.admitMu.Unlock()

	held := d.reaper.heldNames()

	var wanted int
	for _, t := range targets {
		if !held[t.Name] {
			wanted++
		}
	}
	if wanted == 0 {
		for _, t := range targets {
			d.reaper.hold(t)
		}
		return nil
	}

	// Room on the disk before room in memory: a host that fills up takes every
	// run on it with it, not just the one that would have been started.
	if err := d.checkDisk(ctx); err != nil {
		return err
	}

	capacity := d.capacityLimit(ctx)
	if len(held)+wanted > capacity {
		return fmt.Errorf("%w: %d of %d machines in use, %d more needed",
			broker.ErrAtCapacity, len(held), capacity, wanted)
	}

	// Held under the same lock the count was taken under, so two connections
	// arriving together cannot both be told there is room for one.
	for _, t := range targets {
		d.reaper.hold(t)
	}
	return nil
}

// capacityLimit is how many machines this node will run at once.
func (d *Docker) capacityLimit(ctx context.Context) int {
	d.capacityOnce.Do(func() {
		if d.maxMachines > 0 {
			d.capacity = d.maxMachines
			d.log.Info("capacity set", "machines", d.capacity, "source", "configured")
			return
		}
		d.capacity = d.deriveCapacity(ctx)
		d.log.Info("capacity derived from engine memory", "machines", d.capacity)
	})
	return d.capacity
}

// deriveCapacity works out how many machines fit in the engine's memory.
//
// Asking the engine rather than reading /proc/meminfo is deliberate: the
// daemon may not be on this machine, and what matters is the memory where the
// containers will actually run.
func (d *Docker) deriveCapacity(ctx context.Context) int {
	var info struct {
		MemTotal int64 `json:"MemTotal"`
	}
	if err := d.api.Get(ctx, "/info", &info); err != nil {
		d.log.Error("read engine memory; falling back to the minimum capacity", "error", err)
		return MinCapacity
	}

	return capacityFor(info.MemTotal, d.limits.Memory)
}

// capacityFor is how many machines of a given size fit in a given amount of
// memory, with headroom left over.
func capacityFor(memTotal, perMachine int64) int {
	if memTotal <= 0 || perMachine <= 0 {
		return MinCapacity
	}
	if capacity := int(float64(memTotal) * memoryHeadroom / float64(perMachine)); capacity > MinCapacity {
		return capacity
	}
	return MinCapacity
}
