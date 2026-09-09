package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/klahr/wge/internal/build"
	"github.com/klahr/wge/internal/docker"
	"github.com/klahr/wge/internal/manifest"
)

// sweepImage is any image that stays up; the sweep does not care what is in it.
const sweepImage = "wge/debian-13"

type stubImages struct{}

func (stubImages) Image(string, int, string) (string, error) { return sweepImage, nil }

type stubSeeder struct{}

func (stubSeeder) Seed(context.Context, string, *manifest.Game, string, *build.RunSecrets) error {
	return nil
}

// recordingRuns captures the placement calls the runtime makes.
type recordingRuns struct {
	set   chan string
	runID int64
}

func (r *recordingRuns) SetHost(_ context.Context, runID int64, node string) error {
	r.runID = runID
	select {
	case r.set <- node:
	default:
	}
	return nil
}

func requireEngine(t *testing.T) *docker.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("talks to the Docker engine; skipped in short mode")
	}

	api := docker.New("")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var info struct{ ID string }
	if err := api.Get(ctx, "/images/"+sweepImage+"/json", &info); err != nil {
		t.Skipf("image %s is not available: %v", sweepImage, err)
	}
	return api
}

// startLabelled creates a container carrying the labels a game container has,
// standing in for one a previous engine process left behind.
func startLabelled(t *testing.T, api *docker.Client, name string, runID int64) {
	t.Helper()
	ctx := context.Background()

	_ = api.Delete(ctx, "/containers/"+name+"?force=true&v=true")

	body := map[string]any{
		"Image": sweepImage,
		"Labels": map[string]string{
			"wge.run":  fmt.Sprint(runID),
			"wge.game": "test",
			"wge.host": "main",
		},
		"HostConfig": map[string]any{"NetworkMode": "none"},
	}
	if err := api.Post(ctx, "/containers/create?name="+name, body, nil); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if err := api.Post(ctx, "/containers/"+name+"/start", nil, nil); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = api.Delete(context.Background(), "/containers/"+name+"?force=true&v=true")
	})
}

func exists(t *testing.T, api *docker.Client, name string) bool {
	t.Helper()
	var out struct{ Id string }
	err := api.Get(context.Background(), "/containers/"+name+"/json", &out)
	if err == nil {
		return true
	}
	if docker.IsNotFound(err) {
		return false
	}
	t.Fatalf("inspect %s: %v", name, err)
	return false
}

func newTestDocker(t *testing.T, runs Runs) *Docker {
	t.Helper()
	d, err := NewDocker(Options{
		Images: stubImages{},
		Seeder: stubSeeder{},
		Runs:   runs,
		Node:   "test-node",
		Grace:  testGrace,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewDocker: %v", err)
	}
	return d
}

// A container left by a previous engine process has nothing holding it, and
// nothing can tell whether a player is behind it. Collecting it is right:
// rebuilding costs one reconnection, and leaking it costs the host.
func TestSweepCollectsContainersLeftByAPreviousProcess(t *testing.T) {
	api := requireEngine(t)
	runs := &recordingRuns{set: make(chan string, 4)}
	d := newTestDocker(t, runs)

	const name = "wge-run-9001-main"
	startLabelled(t, api, name, 9001)
	if !exists(t, api, name) {
		t.Fatal("the fixture container did not start")
	}

	d.sweepOnce(context.Background())

	if exists(t, api, name) {
		t.Fatal("an abandoned container survived the sweep")
	}

	// And the run is no longer pinned to this machine.
	select {
	case node := <-runs.set:
		if node != "" {
			t.Fatalf("placement set to %q, want it cleared", node)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the run's placement was never cleared")
	}
}

// The sweep must not take a container out from under a live session.
func TestSweepSparesHeldContainers(t *testing.T) {
	api := requireEngine(t)
	d := newTestDocker(t, &recordingRuns{set: make(chan string, 4)})

	const name = "wge-run-9002-main"
	startLabelled(t, api, name, 9002)

	spot := target{Name: name, RunID: 9002}
	d.reaper.hold(spot)
	defer d.reaper.forget(name)

	d.sweepOnce(context.Background())

	if !exists(t, api, name) {
		t.Fatal("the sweep destroyed a container with a live session")
	}
}

// A container inside its grace period is spoken for too: the player may still
// be reconnecting.
func TestSweepSparesContainersAwaitingTheirGracePeriod(t *testing.T) {
	api := requireEngine(t)
	d := newTestDocker(t, &recordingRuns{set: make(chan string, 4)})

	const name = "wge-run-9003-main"
	startLabelled(t, api, name, 9003)

	// Long grace so the timer cannot fire during the test.
	d.reaper = newReaper(time.Hour, d.destroy)
	spot := target{Name: name, RunID: 9003}
	d.reaper.hold(spot)
	d.reaper.release(spot)
	defer d.reaper.stop()

	d.sweepOnce(context.Background())

	if !exists(t, api, name) {
		t.Fatal("the sweep collected a container inside its grace period")
	}
}
