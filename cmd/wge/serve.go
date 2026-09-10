package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/klahr/wge/internal/broker"
	"github.com/klahr/wge/internal/build"
	"github.com/klahr/wge/internal/library"
	"github.com/klahr/wge/internal/runtime"
	"github.com/klahr/wge/internal/store"

	"golang.org/x/crypto/ssh"
)

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":2222", "address to listen on")
	gamesDir := fs.String("games", "games", "directory of game definitions")
	only := fs.String("game", "", "games to serve, by id, comma-separated (default: every game under -games)")
	dbPath := fs.String("db", "wge.db", "path to the engine database")
	grace := fs.Duration("grace", runtime.DefaultGrace, "how long a container outlives its last session")
	sweep := fs.Duration("sweep", runtime.DefaultSweep, "how often abandoned containers are collected")
	node := fs.String("node", "", "name of this machine in the runs table (default: hostname)")
	nodes := fs.String("nodes", "", "machines to run containers on, as name=endpoint pairs (default: this one)")
	noScratch := fs.Bool("no-scratch", false, "do not give runs a persistent scratch volume")
	maxMachines := fs.Int("max-machines", 0, "most containers to run at once (default: derived from the engine's memory)")
	containerRuntime := fs.String("container-runtime", "", "OCI runtime for game containers, e.g. runsc (default: the engine's own)")
	storageSize := fs.Int64("storage-size", 0, "bytes a container may write to its own filesystem (0: uncapped; verified at startup)")
	scratchSize := fs.Int64("scratch-size", 0, "bytes a run may keep in its scratch space before the player is told to clear some")
	minFree := fs.Int64("min-free", 0, "bytes of disk to keep back; no new run is started below it")
	open := fs.Bool("open", false, "let anybody enrol, without an invitation")
	enrollLimit := fs.Int("enroll-limit", 0, "enrolment attempts allowed per address per window (-1 for no limit)")
	enrollWindow := fs.Duration("enroll-window", broker.DefaultEnrollWindow, "the window the enrolment limit applies over")
	hostKeyPath := fs.String("host-key", "host_key", "SSH host key; generated if absent")
	socket := fs.String("docker", runtime.DefaultSocket, "Docker engine socket")
	verbose := fs.Bool("v", false, "log at debug level")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := applyEnv(fs); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	lib, err := library.Load(*gamesDir)
	if err != nil {
		return err
	}
	if ids := splitList(*only); len(ids) > 0 {
		if lib, err = lib.Only(ids); err != nil {
			return err
		}
	}
	for _, g := range lib.All() {
		log.Info("game loaded", "id", g.ID, "version", g.Version, "levels", len(g.Levels))
	}

	st, err := store.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	hostKey, err := loadOrCreateHostKey(*hostKeyPath)
	if err != nil {
		return err
	}

	parsed, err := parseNodes(*nodes)
	if err != nil {
		return err
	}

	rt, err := runtime.NewDocker(runtime.Options{
		Socket:           *socket,
		Images:           lib,
		Seeder:           build.NewSeeder(),
		Runs:             st,
		Node:             *node,
		NoScratch:        *noScratch,
		MaxMachines:      *maxMachines,
		ContainerRuntime: *containerRuntime,
		Nodes:            parsed,
		Limits:           limitsFrom(*storageSize, *scratchSize, *minFree),
		Grace:            *grace,
		Sweep:            *sweep,
		Logger:           log,
	})
	if err != nil {
		return err
	}

	if err := requireImages(ctx, rt, lib); err != nil {
		return err
	}
	if err := requireRuntime(ctx, rt, *containerRuntime); err != nil {
		return err
	}
	if err := requireSetuid(ctx, rt, lib, *containerRuntime); err != nil {
		return err
	}
	if err := requireStorageQuota(ctx, rt, lib); err != nil {
		return err
	}

	srv, err := broker.New(broker.Config{
		Addr:           *addr,
		HostKey:        hostKey,
		Store:          st,
		Games:          lib,
		Runtime:        rt,
		Logger:         log,
		OpenEnrollment: *open,
		EnrollLimit:    *enrollLimit,
		EnrollWindow:   *enrollWindow,
	})
	if err != nil {
		return err
	}

	bound, err := srv.Listen()
	if err != nil {
		return err
	}

	// Collect abandoned containers for as long as the broker is serving. The
	// first pass runs immediately and clears whatever a previous process left.
	//
	// It starts once the listener is bound, so a process that never becomes
	// the server does not reap the containers of the one that is, and a
	// startup that fails here does not report a cancelled sweep as an error.
	go rt.Run(ctx)

	log.Info("broker listening", "addr", bound.String(),
		"fingerprint", ssh.FingerprintSHA256(hostKey.PublicKey()),
		"enrollment", enrollmentMode(*open),
		"container-runtime", runtimeName(*containerRuntime),
		"machines", strings.Join(rt.Names(), ","))

	return srv.Serve(ctx)
}

// requireImages refuses to serve a game whose image was never built.
//
// The alternative is a server that starts, reports itself healthy, and fails
// at the moment a player presents a correct password -- which reads to them as
// a broken game and to whoever deployed it as nothing at all.
func requireImages(ctx context.Context, rt *runtime.Docker, lib *library.Library) error {
	var want []string
	source := map[string]string{}

	for _, g := range lib.All() {
		for _, h := range g.HostIDs() {
			image := library.ImageName(g.ID, g.Version, h)
			want = append(want, image)
			source[image] = g.Dir
		}
	}

	missing, err := rt.MissingImages(ctx, want)
	if err != nil {
		return fmt.Errorf("check game images: %w", err)
	}
	if len(missing) == 0 {
		return nil
	}

	// One line per game rather than per image: a two-machine game is one
	// build, and telling somebody to run it twice is telling them wrong.
	var b strings.Builder
	fmt.Fprintf(&b, "%d game image(s) have not been built:\n", len(missing))
	for _, image := range missing {
		fmt.Fprintf(&b, "  %s\n", image)
	}

	seen := map[string]bool{}
	b.WriteString("\nbuild them with:\n")
	for _, image := range missing {
		if dir := source[image]; !seen[dir] {
			seen[dir] = true
			fmt.Fprintf(&b, "  wge build %s\n", dir)
		}
	}
	return errors.New(strings.TrimRight(b.String(), "\n"))
}

// requireRuntime refuses to serve with a runtime the engine does not have.
func requireRuntime(ctx context.Context, rt *runtime.Docker, want string) error {
	if want == "" {
		return nil
	}

	known, err := rt.Runtimes(ctx)
	if err != nil {
		return err
	}
	if known[want] {
		return nil
	}

	var names []string
	for name := range known {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Errorf("the engine has no runtime %q; it knows: %s",
		want, strings.Join(names, ", "))
}

// requireSetuid refuses to serve a game whose levels cannot be moved between.
//
// The check exists because the failure it catches is silent. gVisor ignores
// the setuid bit unless it is started with --allow-suid: everything boots,
// every service runs, and the only thing that does not work is su(1), which is
// how a player gets from one level to the next.
func requireSetuid(ctx context.Context, rt *runtime.Docker, lib *library.Library, containerRuntime string) error {
	games := lib.All()
	if len(games) == 0 {
		return nil
	}
	g := games[0]
	image := library.ImageName(g.ID, g.Version, g.HostIDs()[0])

	err := rt.VerifySetuid(ctx, image)
	if err == nil {
		return nil
	}
	if !errors.Is(err, runtime.ErrSetuidIgnored) {
		return fmt.Errorf("check that setuid works: %w", err)
	}

	name := containerRuntime
	if name == "" {
		name = "the engine's default runtime"
	}
	return fmt.Errorf(`%w

su(1) is how a player moves between levels, and it cannot work: %s does not
let a setuid binary elevate. gVisor does this unless it is registered with
--allow-suid, for example in /etc/docker/daemon.json:

    "runtimes": {
      "runsc": {
        "path": "/usr/bin/runsc",
        "runtimeArgs": ["--allow-suid"]
      }
    }

then: sudo systemctl reload docker`, err, name)
}

// limitsFrom starts from the defaults and applies whatever was configured.
func limitsFrom(storage, scratch, minFree int64) runtime.Limits {
	l := runtime.DefaultLimits()
	if storage > 0 {
		l.StorageBytes = storage
	}
	if scratch > 0 {
		l.ScratchBytes = scratch
	}
	if minFree > 0 {
		l.MinFreeBytes = minFree
	}
	return l
}

// requireStorageQuota refuses to serve with a size limit that does nothing.
//
// Docker accepts --storage-opt size on drivers that ignore it: overlayfs on
// ext4 takes the option, reports success, and lets a container write until the
// disk is full. An operator who configured a limit should find out here, not
// from a full disk.
func requireStorageQuota(ctx context.Context, rt *runtime.Docker, lib *library.Library) error {
	games := lib.All()
	if len(games) == 0 {
		return nil
	}
	g := games[0]
	image := library.ImageName(g.ID, g.Version, g.HostIDs()[0])

	err := rt.VerifyStorageQuota(ctx, image)
	if err == nil {
		return nil
	}
	if !errors.Is(err, runtime.ErrStorageQuotaIgnored) {
		return fmt.Errorf("check the storage quota: %w", err)
	}
	return fmt.Errorf(`%w

A per-container size limit needs a storage driver that enforces one: overlay2
on XFS with pquota, or btrfs. Leave -storage-size unset to run without it --
the host is still protected by -min-free, which stops new runs before the disk
fills`, err)
}

// parseNodes reads the machines a pool is made of.
//
//	-nodes "relay=tcp://10.0.0.11:2375,vault=tcp://10.0.0.12:2375"
//
// A name is what a run's placement is recorded as, so it has to outlive a
// restart and mean the same machine afterwards: change the endpoint and the
// runs follow it, change the name and they do not.
func parseNodes(spec string) ([]runtime.Node, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}

	var out []runtime.Node
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, endpoint, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("node %q is not name=endpoint", pair)
		}
		name, endpoint = strings.TrimSpace(name), strings.TrimSpace(endpoint)
		if name == "" || endpoint == "" {
			return nil, fmt.Errorf("node %q is not name=endpoint", pair)
		}
		out = append(out, runtime.Node{Name: name, Socket: endpoint})
	}
	return out, nil
}

// splitList reads a comma-separated flag value, in the shape -nodes takes.
func splitList(spec string) []string {
	var out []string
	for _, item := range strings.Split(spec, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func enrollmentMode(open bool) string {
	if open {
		return "open"
	}
	return "invitation only"
}

func runtimeName(name string) string {
	if name == "" {
		return "engine default"
	}
	return name
}

// loadOrCreateHostKey keeps the host key stable across restarts. A key that
// changed on every boot would make every player's client warn about a
// man-in-the-middle, and teach them to ignore the warning.
func loadOrCreateHostKey(path string) (ssh.Signer, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		signer, err := ssh.ParsePrivateKey(raw)
		if err != nil {
			return nil, fmt.Errorf("parse host key %s: %w", path, err)
		}
		return signer, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read host key %s: %w", path, err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, err
	}

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, fmt.Errorf("write host key %s: %w", path, err)
	}
	return ssh.NewSignerFromKey(priv)
}
