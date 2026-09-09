package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
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
	hostKeyPath := fs.String("host-key", "host_key", "SSH host key; generated if absent")
	socket := fs.String("docker", runtime.DefaultSocket, "Docker engine socket")
	verbose := fs.Bool("v", false, "log at debug level")
	if err := fs.Parse(args); err != nil {
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
		Socket: *socket,
		Images: lib,
		Seeder: build.NewSeeder(*socket),
		Logger: log,
	})
	if err != nil {
		return err
	}

	srv, err := broker.New(broker.Config{
		Addr:    *addr,
		HostKey: hostKey,
		Store:   st,
		Games:   lib,
		Runtime: rt,
		Logger:  log,
	})
	if err != nil {
		return err
	}

	bound, err := srv.Listen()
	if err != nil {
		return err
	}
	log.Info("broker listening", "addr", bound.String(),
		"fingerprint", ssh.FingerprintSHA256(hostKey.PublicKey()))

	return srv.Serve(ctx)
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
