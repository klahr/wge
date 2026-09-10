package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/klahr/wge/internal/broker"
	"github.com/klahr/wge/internal/build"
	"github.com/klahr/wge/internal/docker"
	"github.com/klahr/wge/internal/library"
	"github.com/klahr/wge/internal/manifest"
	"github.com/klahr/wge/internal/store"
)

// sweepImage is any image that stays up; the sweep does not care what is in it.
const sweepImage = "wge/debian-13"

type stubImages struct{}

func (stubImages) Image(string, int, string) (string, error) { return sweepImage, nil }

type stubSeeder struct{}

func (stubSeeder) Seed(context.Context, string, *manifest.Game, string, *build.RunSecrets) error {
	return nil
}

// recordingRuns captures the placement and progress calls the runtime makes.
type recordingRuns struct {
	set   chan string
	runID int64

	mu       sync.Mutex
	progress []string
}

func (r *recordingRuns) RecordProgress(_ context.Context, _ int64, levelID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.progress = append(r.progress, levelID)
	return nil
}

func (r *recordingRuns) reached() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.progress...)
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
		unavailable(t, "image %s is not available: %v", sweepImage, err)
	}
	return api
}

// unavailable skips, or fails when the environment has promised Docker.
//
// These tests skip themselves when there is no engine, which is right on a
// laptop and wrong in CI: a run that skipped everything reports the same green
// as a run that proved something.
func unavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("WGE_REQUIRE_DOCKER") != "" {
		t.Fatalf("WGE_REQUIRE_DOCKER is set but "+format, args...)
	}
	t.Skipf(format, args...)
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

// The end-to-end shape of progress collection: a machine boots, a session is
// opened on it from inside, and the level is recorded without the front door
// ever having attached anybody to it.
func TestProgressIsCollectedFromInsideTheRun(t *testing.T) {
	api := requireEngine(t)
	ctx := context.Background()

	game, err := manifest.Load(filepath.Join("..", "..", "games", "heist"))
	if err != nil {
		t.Fatalf("load reference game: %v", err)
	}

	const runID = 9101
	image := library.ImageName(game.ID, game.Version, "relay2")

	var info struct{ ID string }
	if err := api.Get(ctx, "/images/"+image+"/json", &info); err != nil {
		unavailable(t, "image %s is not available: %v", image, err)
	}

	runs := &recordingRuns{set: make(chan string, 4)}
	d := newTestDocker(t, runs)

	name := ContainerName(runID, "relay2")
	startFromImage(t, api, name, image, runID, "relay2")

	d.waitForBoot(ctx, name)
	d.markLive(ctx, name)

	// Nothing has happened yet, so nothing should be claimed.
	d.collectProgress(sessionFor(game, runID))
	if got := runs.reached(); len(got) != 0 {
		t.Fatalf("levels reported before anything happened: %v", got)
	}

	// A transition made from inside the machine, which the front door cannot
	// see: root opening a session as the backup operator.
	if _, err := api.Exec(ctx, name, docker.ExecOptions{
		Cmd: []string{"su", "-", "bkup", "-c", "true"}, User: "root",
	}); err != nil {
		t.Fatalf("su inside the container: %v", err)
	}

	d.collectProgress(sessionFor(game, runID))

	var found bool
	for _, level := range runs.reached() {
		if level == "backup-op" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the level reached from inside was not recorded; got %v", runs.reached())
	}
}

// sessionFor builds the minimum a progress collection needs.
func sessionFor(game *manifest.Game, runID int64) *broker.Session {
	return &broker.Session{
		Game: game,
		Run:  &store.Run{ID: runID, GameID: game.ID, GameVersion: game.Version},
	}
}

// startFromImage brings up a real game image under the labels the engine uses.
func startFromImage(t *testing.T, api *docker.Client, name, image string, runID int64, host string) {
	t.Helper()
	ctx := context.Background()

	_ = api.Delete(ctx, "/containers/"+name+"?force=true&v=true")

	body := map[string]any{
		"Image":    image,
		"Hostname": host,
		"Labels": map[string]string{
			"wge.run": fmt.Sprint(runID), "wge.game": "heist", "wge.host": host,
		},
		"HostConfig": map[string]any{
			"NetworkMode": "none",
			"CapDrop":     []string{"ALL"},
			"CapAdd": []string{
				"SETUID", "SETGID", "CHOWN", "FOWNER", "FSETID",
				"DAC_OVERRIDE", "KILL", "AUDIT_WRITE", "NET_BIND_SERVICE", "SYS_CHROOT",
			},
		},
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

// A reap takes the machines and leaves the notes. This is the whole point of
// the volume: everything else about a box is a pure function of the salt and
// costs nothing to rebuild, and the player's own files are not.
func TestReapingKeepsTheScratchVolume(t *testing.T) {
	api := requireEngine(t)
	ctx := context.Background()
	d := newTestDocker(t, &recordingRuns{set: make(chan string, 4)})

	const runID = 9201
	name := ContainerName(runID, "main")
	volume := ScratchVolume(runID)

	labels := map[string]string{"wge.run": fmt.Sprint(runID)}
	if err := api.CreateVolume(ctx, volume, labels); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	t.Cleanup(func() { _ = api.RemoveVolume(context.Background(), volume) })

	startLabelled(t, api, name, runID)

	d.sweepOnce(ctx)

	if exists(t, api, name) {
		t.Fatal("the machine survived the sweep")
	}

	volumes, err := api.ListVolumes(ctx, "wge.run="+fmt.Sprint(runID))
	if err != nil {
		t.Fatalf("list volumes: %v", err)
	}
	if len(volumes) != 1 {
		t.Fatalf("the sweep took the scratch volume with the machines; found %d", len(volumes))
	}

	// And a reset is what removes it.
	if err := d.RemoveScratch(ctx, runID); err != nil {
		t.Fatalf("RemoveScratch: %v", err)
	}
	volumes, _ = api.ListVolumes(ctx, "wge.run="+fmt.Sprint(runID))
	if len(volumes) != 0 {
		t.Fatalf("RemoveScratch left %d volumes", len(volumes))
	}
}

// A reset takes everything, including the notes a reap deliberately keeps.
func TestDestroyRunTakesMachinesNetworksAndScratch(t *testing.T) {
	api := requireEngine(t)
	ctx := context.Background()
	runs := &recordingRuns{set: make(chan string, 4)}
	d := newTestDocker(t, runs)

	const runID = 9301
	name := ContainerName(runID, "main")
	volume := ScratchVolume(runID)
	network := RunNetwork(runID)
	labels := map[string]string{"wge.run": fmt.Sprint(runID)}

	if err := api.CreateVolume(ctx, volume, labels); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	t.Cleanup(func() { _ = api.RemoveVolume(context.Background(), volume) })

	if err := api.CreateNetwork(ctx, network, labels); err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() { _ = api.RemoveNetwork(context.Background(), network) })

	startLabelled(t, api, name, runID)

	// A live claim must not save it: a reset is explicit, and the player asked.
	d.reaper.hold(target{Name: name, RunID: runID})

	if err := d.DestroyRun(ctx, runID); err != nil {
		t.Fatalf("DestroyRun: %v", err)
	}

	if exists(t, api, name) {
		t.Error("the machine survived the reset")
	}

	volumes, err := api.ListVolumes(ctx, "wge.run="+fmt.Sprint(runID))
	if err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 0 {
		t.Errorf("the reset left %d scratch volumes; the old notes hold the old answers", len(volumes))
	}

	networks, err := api.ListNetworks(ctx, "wge.run="+fmt.Sprint(runID))
	if err != nil {
		t.Fatal(err)
	}
	if len(networks) != 0 {
		t.Errorf("the reset left %d networks", len(networks))
	}

	// The reaper must not be left holding a name that no longer exists.
	if d.reaper.heldNames()[name] {
		t.Error("the reaper still holds a destroyed machine")
	}
}

// The preflight has to pass under the runtime the tests are running on, or
// every game served by it is broken in the one way nobody thinks to check.
func TestSetuidPreflightPassesUnderTheDefaultRuntime(t *testing.T) {
	requireEngine(t)
	d := newTestDocker(t, &recordingRuns{set: make(chan string, 1)})

	if err := d.VerifySetuid(context.Background(), sweepImage); err != nil {
		t.Fatalf("setuid does not work under this runtime, so su(1) cannot: %v", err)
	}
}

// The preflight must leave nothing behind: it runs on every start.
func TestSetuidPreflightCleansUpAfterItself(t *testing.T) {
	requireEngine(t)
	api := requireEngine(t)
	ctx := context.Background()
	d := newTestDocker(t, &recordingRuns{set: make(chan string, 1)})

	if err := d.VerifySetuid(ctx, sweepImage); err != nil {
		t.Fatalf("VerifySetuid: %v", err)
	}

	left, err := api.ListContainers(ctx, "wge.preflight")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("the preflight left %d containers behind", len(left))
	}

	// And no volume: a preflight is not a run, and run zero has no notes.
	volumes, err := api.ListVolumes(ctx, "wge.run")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range volumes {
		if v.Name == ScratchVolume(0) {
			t.Fatalf("the preflight created %s", v.Name)
		}
	}
}
