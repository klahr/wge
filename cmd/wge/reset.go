package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/klahr/wge/internal/build"
	"github.com/klahr/wge/internal/library"
	"github.com/klahr/wge/internal/runtime"
	"github.com/klahr/wge/internal/store"
)

// cmdReset starts a player's game over.
//
// It exists for the case the derivation was designed around: a run that has
// been spoiled. Nothing has to be rebuilt and no image is touched -- the salt
// is re-rolled and the same puzzles come back with different answers.
func cmdReset(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	dbPath := fs.String("db", "wge.db", "path to the engine database")
	gamesDir := fs.String("games", "games", "directory of game definitions")
	socket := fs.String("docker", runtime.DefaultSocket, "Docker engine socket")
	nodes := fs.String("nodes", "", "machines the run may be on, as name=endpoint pairs (default: this one)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := applyEnv(fs); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: wge reset [-db path] [-games dir] [-nodes spec] <handle> <game>")
	}
	handle, gameID := fs.Arg(0), fs.Arg(1)

	// The same pool the front door serves. A reset that only reached this
	// machine would re-roll the salt while a box carrying the old credentials
	// was still up somewhere else -- two answers to the same puzzle, and the
	// stale one still accepting logins.
	parsed, err := parseNodes(*nodes)
	if err != nil {
		return err
	}

	ctx := context.Background()

	lib, err := library.Load(*gamesDir)
	if err != nil {
		return err
	}
	game, ok := lib.Game(gameID)
	if !ok {
		return fmt.Errorf("no game %q", gameID)
	}

	st, err := openStore(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	player, err := st.PlayerByHandle(ctx, handle)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no player %q", handle)
	}
	if err != nil {
		return err
	}

	run, err := st.Run(ctx, player.ID, game.ID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%s has no run of %s", handle, game.ID)
	}
	if err != nil {
		return err
	}

	rt, err := runtime.NewDocker(runtime.Options{
		Socket: *socket,
		Nodes:  parsed,
		Images: lib,
		Seeder: build.NewSeeder(),
		Runs:   st,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		return err
	}

	// The machines first. Re-rolling the salt while a box carrying the old
	// credentials is still up would leave two answers to the same puzzle.
	if err := rt.DestroyRun(ctx, run.ID); err != nil {
		return fmt.Errorf("take the machines down: %w", err)
	}
	if err := st.ResetRun(ctx, run.ID, game.Version); err != nil {
		return fmt.Errorf("reset run: %w", err)
	}

	fresh, err := st.Run(ctx, player.ID, game.ID)
	if err != nil {
		return err
	}
	entry, ok := game.Entry()
	if !ok {
		return fmt.Errorf("game %s has no entry level", game.ID)
	}

	fmt.Printf("%s: %s reset to version %d\n", handle, game.ID, fresh.GameVersion)
	fmt.Printf("  progress cleared, scratch erased, machines taken down\n")
	fmt.Printf("  first account %s, password %s\n", entry.User, fresh.Deriver().Password(entry.ID))

	if fresh.Salt == run.Salt {
		fmt.Fprintln(os.Stderr, "  warning: the salt did not change")
	}
	return nil
}
