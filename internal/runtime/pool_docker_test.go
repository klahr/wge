package runtime

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/klahr/wge/internal/broker"
	"github.com/klahr/wge/internal/docker"
	"github.com/klahr/wge/internal/manifest"
	"github.com/klahr/wge/internal/store"
)

// dindImage is a Docker daemon in a container, which is the cheapest way to get
// a genuine second engine on one machine. Placement can be unit-tested against
// fakes, but "the container really was created on the other daemon" cannot.
const dindImage = "docker:28-dind"

// secondDaemon returns the endpoint of an engine that is not the local one.
//
// WGE_TEST_NODE_B points at a real second machine when there is one; otherwise
// a daemon is started here and torn down afterwards.
func secondDaemon(t *testing.T) string {
	t.Helper()

	if endpoint := os.Getenv("WGE_TEST_NODE_B"); endpoint != "" {
		return endpoint
	}

	if _, err := exec.LookPath("docker"); err != nil {
		unavailable(t, "no docker CLI to start a second daemon with: %v", err)
	}

	const name = "wge-test-node-b"
	const port = "12376"
	_ = exec.Command("docker", "rm", "-f", name).Run()

	// A daemon inside a container needs the privileges of one. Nothing in the
	// engine runs like this; the fixture does, because a nested daemon cannot
	// be had any other way.
	start := exec.Command("docker", "run", "-d", "--privileged", "--name", name,
		"-p", "127.0.0.1:"+port+":2375", "-e", "DOCKER_TLS_CERTDIR=",
		dindImage, "--host=tcp://0.0.0.0:2375")
	if out, err := start.CombinedOutput(); err != nil {
		unavailable(t, "start a second daemon: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	endpoint := "tcp://127.0.0.1:" + port
	api := docker.New(endpoint)
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		var version struct{ Version string }
		err := api.Get(ctx, "/version", &version)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			unavailable(t, "the second daemon never answered: %v", err)
		}
		time.Sleep(time.Second)
	}

	// It starts empty, and a machine with no image cannot hold a run.
	copyImage(t, sweepImage, endpoint)
	return endpoint
}

// copyImage moves a locally built image to another engine. The games are built
// on the machine that serves them in production; in a test one daemon has them
// and the other has to be given them.
func copyImage(t *testing.T, image, endpoint string) {
	t.Helper()

	save := exec.Command("docker", "save", image)
	load := exec.Command("docker", "-H", endpoint, "load")

	pipe, err := save.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	load.Stdin = pipe

	if err := save.Start(); err != nil {
		t.Fatalf("docker save %s: %v", image, err)
	}
	if out, err := load.CombinedOutput(); err != nil {
		t.Fatalf("docker load onto %s: %v: %s", endpoint, err, out)
	}
	if err := save.Wait(); err != nil {
		t.Fatalf("docker save %s: %v", image, err)
	}
}

// twoDaemonPool builds a pool over the local engine and a second one.
func twoDaemonPool(t *testing.T, runs Runs) (*Docker, *stubSeeder, *docker.Client, *docker.Client) {
	t.Helper()

	local := requireEngine(t)
	endpoint := secondDaemon(t)

	seeder := &stubSeeder{}
	d, err := NewDocker(Options{
		Images: stubImages{},
		Seeder: seeder,
		Runs:   runs,
		Grace:  testGrace,
		Logger: slog.New(slog.DiscardHandler),
		Nodes: []Node{
			{Name: "a", Socket: DefaultSocket},
			{Name: "b", Socket: endpoint},
		},
	})
	if err != nil {
		t.Fatalf("NewDocker: %v", err)
	}
	return d, seeder, local, docker.New(endpoint)
}

// The point of the whole exercise: a run pinned to the second machine has its
// container created on the second machine's engine, and the first one never
// hears about it.
func TestAPlacedRunIsBuiltOnItsOwnDaemon(t *testing.T) {
	runs := newRecordingRuns()
	pool, seeder, a, b := twoDaemonPool(t, runs)
	ctx := context.Background()

	const runID = 9301
	s := &broker.Session{
		Player: &store.Player{Handle: "tester"},
		Game:   &manifest.Game{ID: "test", Version: 1},
		Run:    &store.Run{ID: runID, GameID: "test", GameVersion: 1, CurrentHost: "b"},
	}

	worker, err := pool.place(ctx, s)
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if worker.name != "b" {
		t.Fatalf("a run placed on b was routed to %s", worker.name)
	}

	name := ContainerName(runID, "main")
	t.Cleanup(func() {
		_ = a.Delete(context.Background(), "/containers/"+name+"?force=true&v=true")
		_ = b.Delete(context.Background(), "/containers/"+name+"?force=true&v=true")
	})

	if err := worker.ensureContainer(ctx, s, "main", nil); err != nil {
		t.Fatalf("ensureContainer on b: %v", err)
	}

	if !exists(t, b, name) {
		t.Error("the container is not on the daemon the run was placed on")
	}
	if exists(t, a, name) {
		t.Error("the container was created on the wrong daemon")
	}

	// And it was seeded on the engine it was created on. A container the pool
	// placed correctly but seeded elsewhere is worse than one it never made:
	// the player is attached to a box full of template text.
	engines := seeder.engines()
	if len(engines) != 1 {
		t.Fatalf("seeded %d times, want once: %v", len(engines), engines)
	}
	if engines[0] != b.Endpoint() {
		t.Errorf("seeded against %s, want the daemon holding the container", engines[0])
	}

	// And the placement it recorded is the machine that actually holds it.
	select {
	case node := <-runs.set:
		if node != "b" {
			t.Errorf("placement recorded as %q, want b", node)
		}
	case <-time.After(5 * time.Second):
		t.Error("the placement was never recorded")
	}

	// A destroy has to find it without being told where it is.
	if err := pool.DestroyRun(ctx, runID); err != nil {
		t.Fatalf("DestroyRun: %v", err)
	}
	if exists(t, b, name) {
		t.Error("the run survived a pool-wide destroy")
	}
}

// A gap on one machine is a gap in the pool, and an operator needs to be told
// which machine to go and fix.
func TestMissingImagesNameTheMachineTheyAreMissingFrom(t *testing.T) {
	pool, _, _, _ := twoDaemonPool(t, newRecordingRuns())

	const absent = "wge/definitely-not-built:latest"
	missing, err := pool.MissingImages(context.Background(), []string{sweepImage, absent})
	if err != nil {
		t.Fatalf("MissingImages: %v", err)
	}

	want := map[string]bool{
		absent + " (on a)": true,
		absent + " (on b)": true,
	}
	got := map[string]bool{}
	for _, m := range missing {
		got[m] = true
	}
	for label := range want {
		if !got[label] {
			t.Errorf("%q was not reported; got %v", label, missing)
		}
	}
	for label := range got {
		if !want[label] {
			t.Errorf("%q was reported missing but both machines have it", label)
		}
	}
}

// A runtime only one machine knows is one a player might or might not get.
func TestRuntimesAreTheOnesEveryMachineHas(t *testing.T) {
	pool, _, _, b := twoDaemonPool(t, newRecordingRuns())
	ctx := context.Background()

	common, err := pool.Runtimes(ctx)
	if err != nil {
		t.Fatalf("Runtimes: %v", err)
	}
	if !common["runc"] {
		t.Fatalf("runc is not in the intersection: %v", common)
	}

	// Whatever the pool offers, every machine must actually have.
	var info struct{ Runtimes map[string]any }
	if err := b.Get(ctx, "/info", &info); err != nil {
		t.Fatalf("inspect the second daemon: %v", err)
	}
	for name := range common {
		if _, ok := info.Runtimes[name]; !ok {
			t.Errorf("the pool offers %q, which the second machine does not have", name)
		}
	}
}
