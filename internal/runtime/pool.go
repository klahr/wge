package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/klahr/wge/internal/broker"
)

// Node names a machine that runs game containers and says how to reach its
// engine.
type Node struct {
	Name string
	// Socket is a unix path or a tcp:// endpoint. Empty takes the default
	// local socket.
	Socket string
}

// Docker places runs on machines and drives their engines.
//
// A run is a pure function of its salt, so any machine could build it -- but
// only the machine holding its containers has its scratch space, and that is
// the one thing about a run that cannot be rebuilt. So a run that has been
// placed stays where it is, and only a new one is scheduled.
type Docker struct {
	nodes  []*node
	byName map[string]*node
	log    *slog.Logger
}

// NewDocker returns a runtime over one machine or several.
func NewDocker(opts Options) (*Docker, error) {
	if opts.Images == nil {
		return nil, fmt.Errorf("runtime: an image resolver is required")
	}
	if opts.Seeder == nil {
		return nil, fmt.Errorf("runtime: a seeder is required; an unseeded container has no credentials in it")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Limits.Memory == 0 && opts.Limits.NanoCPUs == 0 && opts.Limits.PidsLimit == 0 {
		opts.Limits = DefaultLimits()
	}
	if opts.Limits.Capabilities == nil {
		opts.Limits.Capabilities = multiUserCaps
	}
	if opts.Sweep <= 0 {
		opts.Sweep = DefaultSweep
	}

	nodes := opts.Nodes
	if len(nodes) > 0 && opts.Node != "" {
		// One of them would be silently ignored, and which one is not obvious.
		// An operator who set both has a mistaken model of what they configured.
		return nil, fmt.Errorf("runtime: -node names this machine and -nodes names a pool; give one or the other")
	}
	if len(nodes) == 0 {
		// One machine, which is this one. The name matters because it is what
		// a run's placement is recorded as, and it has to survive a restart.
		name := opts.Node
		if name == "" {
			host, err := os.Hostname()
			if err != nil {
				return nil, fmt.Errorf("runtime: determine node name: %w", err)
			}
			name = host
		}
		nodes = []Node{{Name: name, Socket: opts.Socket}}
	}

	d := &Docker{log: opts.Logger, byName: map[string]*node{}}
	for _, n := range nodes {
		if n.Name == "" {
			return nil, fmt.Errorf("runtime: a node has no name")
		}
		if _, dup := d.byName[n.Name]; dup {
			return nil, fmt.Errorf("runtime: node %q is declared twice", n.Name)
		}
		socket := n.Socket
		if socket == "" {
			socket = DefaultSocket
		}

		worker := newNode(n.Name, socket, opts)
		d.nodes = append(d.nodes, worker)
		d.byName[n.Name] = worker
	}
	return d, nil
}

// Names returns the machines in this pool.
func (d *Docker) Names() []string {
	out := make([]string, 0, len(d.nodes))
	for _, n := range d.nodes {
		out = append(out, n.name)
	}
	sort.Strings(out)
	return out
}

// Attach sends the player to the machine holding their run.
func (d *Docker) Attach(ctx context.Context, s *broker.Session) (int, error) {
	worker, err := d.place(ctx, s)
	if err != nil {
		return 0, err
	}
	return worker.Attach(ctx, s)
}

// place decides which machine serves a session.
//
// A run that has been placed goes back to where it was, because its scratch
// space is there and nowhere else. Only a run with no placement is scheduled,
// and it goes to the machine with the most room -- spread rather than packed,
// so losing a machine takes as few players with it as possible.
func (d *Docker) place(ctx context.Context, s *broker.Session) (*node, error) {
	if len(d.nodes) == 1 {
		return d.nodes[0], nil
	}

	if placed := s.Run.CurrentHost; placed != "" {
		if worker, ok := d.byName[placed]; ok {
			return worker, nil
		}
		// The machine it was on is not in the pool any more, so its containers
		// and its scratch went with it. The game is intact -- it is a function
		// of the salt -- but the player's own files are not, and that is worth
		// saying out loud.
		d.log.Warn("run was placed on a machine that is no longer configured",
			"run", s.Run.ID, "was", placed, "scratch", "lost")
	}

	var best *node
	var bestFree int
	for _, worker := range d.nodes {
		held, capacity := worker.load(ctx)
		if free := capacity - held; best == nil || free > bestFree {
			best, bestFree = worker, free
		}
	}
	if best == nil {
		return nil, fmt.Errorf("%w: no machines configured", broker.ErrAtCapacity)
	}
	return best, nil
}

// Run collects abandoned machines on every node until the context is cancelled.
func (d *Docker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, worker := range d.nodes {
		wg.Add(1)
		go func(w *node) {
			defer wg.Done()
			w.Run(ctx)
		}(worker)
	}
	wg.Wait()
}

// Runtimes returns the OCI runtimes every machine in the pool has.
//
// The intersection rather than the union: a runtime only some of them know is
// one a player might or might not get, depending on where they land.
func (d *Docker) Runtimes(ctx context.Context) (map[string]bool, error) {
	var common map[string]bool

	for _, worker := range d.nodes {
		known, err := worker.Runtimes(ctx)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", worker.name, err)
		}
		if common == nil {
			common = known
			continue
		}
		for name := range common {
			if !known[name] {
				delete(common, name)
			}
		}
	}
	return common, nil
}

// MissingImages returns the images any machine in the pool does not have.
//
// Named by machine when there is more than one, because "build it" is a
// different instruction depending on where the gap is.
func (d *Docker) MissingImages(ctx context.Context, images []string) ([]string, error) {
	seen := map[string]bool{}
	var missing []string

	for _, worker := range d.nodes {
		gaps, err := worker.MissingImages(ctx, images)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", worker.name, err)
		}
		for _, image := range gaps {
			label := image
			if len(d.nodes) > 1 {
				label = image + " (on " + worker.name + ")"
			}
			if !seen[label] {
				seen[label] = true
				missing = append(missing, label)
			}
		}
	}
	return missing, nil
}

// VerifySetuid checks every machine: a pool is only as usable as its worst
// member, and a player does not choose which one they land on.
func (d *Docker) VerifySetuid(ctx context.Context, image string) error {
	return d.everyNode(func(worker *node) error { return worker.VerifySetuid(ctx, image) })
}

// VerifyStorageQuota checks every machine, for the same reason.
func (d *Docker) VerifyStorageQuota(ctx context.Context, image string) error {
	return d.everyNode(func(worker *node) error { return worker.VerifyStorageQuota(ctx, image) })
}

// DestroyRun removes a run from wherever it is.
//
// Every machine, not just the one it is placed on: a run that moved, or was
// left behind by a pool that has since been reconfigured, has leftovers where
// nothing is looking for them.
func (d *Docker) DestroyRun(ctx context.Context, runID int64) error {
	return d.everyNode(func(worker *node) error { return worker.DestroyRun(ctx, runID) })
}

// RemoveScratch deletes a run's scratch space wherever it is.
func (d *Docker) RemoveScratch(ctx context.Context, runID int64) error {
	return d.everyNode(func(worker *node) error { return worker.RemoveScratch(ctx, runID) })
}

func (d *Docker) everyNode(do func(*node) error) error {
	var failures []error
	for _, worker := range d.nodes {
		if err := do(worker); err != nil {
			if len(d.nodes) > 1 {
				err = fmt.Errorf("node %s: %w", worker.name, err)
			}
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return errorList(failures)
}

// errorList reports every machine that failed rather than the first, so one
// broken node does not hide another.
type errorList []error

func (e errorList) Error() string {
	parts := make([]string, 0, len(e))
	for _, err := range e {
		parts = append(parts, err.Error())
	}
	return strings.Join(parts, "; ")
}

// Unwrap keeps errors.Is working through the list. The callers that classify
// these -- a setuid bit that does not elevate, a storage limit that does
// nothing -- ask what kind of failure it was, and a wrapper that swallowed
// that would turn a precise refusal back into a generic one.
func (e errorList) Unwrap() []error { return e }
