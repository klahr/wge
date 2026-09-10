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
	dbPath := fs.String("db", "wge.db", "path to the engine database")
	grace := fs.Duration("grace", runtime.DefaultGrace, "how long a container outlives its last session")
	sweep := fs.Duration("sweep", runtime.DefaultSweep, "how often abandoned containers are collected")
	node := fs.String("node", "", "name of this machine in the runs table (default: hostname)")
	noScratch := fs.Bool("no-scratch", false, "do not give runs a persistent scratch volume")
	maxMachines := fs.Int("max-machines", 0, "most containers to run at once (default: derived from the engine's memory)")
	containerRuntime := fs.String("container-runtime", "", "OCI runtime for game containers, e.g. runsc (default: the engine's own)")
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

	rt, err := runtime.NewDocker(runtime.Options{
		Socket:           *socket,
		Images:           lib,
		Seeder:           build.NewSeeder(*socket),
		Runs:             st,
		Node:             *node,
		NoScratch:        *noScratch,
		MaxMachines:      *maxMachines,
		ContainerRuntime: *containerRuntime,
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

	// Collect abandoned containers for as long as the broker is serving. The
	// first pass runs immediately and clears whatever a previous process left.
	go rt.Run(ctx)

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
	log.Info("broker listening", "addr", bound.String(),
		"fingerprint", ssh.FingerprintSHA256(hostKey.PublicKey()),
		"enrollment", enrollmentMode(*open),
		"container-runtime", runtimeName(*containerRuntime))

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
