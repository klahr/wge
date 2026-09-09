package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func open(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "wge.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, ctx
}

func TestPlayerLookupByKey(t *testing.T) {
	s, ctx := open(t)

	created, err := s.CreatePlayer(ctx, "rook", "SHA256:aaa")
	if err != nil {
		t.Fatalf("CreatePlayer: %v", err)
	}

	got, err := s.PlayerByKey(ctx, "SHA256:aaa")
	if err != nil {
		t.Fatalf("PlayerByKey: %v", err)
	}
	if got.ID != created.ID || got.Handle != "rook" {
		t.Fatalf("got %+v, want %+v", got, created)
	}

	if _, err := s.PlayerByKey(ctx, "SHA256:unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown key: got %v, want ErrNotFound", err)
	}

	// A second key on the same player resolves to the same identity, so a
	// player can connect from another machine mid-run.
	if err := s.AddKey(ctx, created.ID, "SHA256:bbb"); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	again, err := s.PlayerByKey(ctx, "SHA256:bbb")
	if err != nil || again.ID != created.ID {
		t.Fatalf("second key resolved to %+v (%v)", again, err)
	}
}

func TestHandlesAreUnique(t *testing.T) {
	s, ctx := open(t)
	if _, err := s.CreatePlayer(ctx, "rook", "SHA256:aaa"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePlayer(ctx, "rook", "SHA256:bbb"); err == nil {
		t.Fatal("expected a duplicate handle to be rejected")
	}
}

// The resume property at the storage layer: the salt written today is the salt
// read tomorrow, so tomorrow's container derives today's passwords.
func TestRunSaltSurvivesReload(t *testing.T) {
	s, ctx := open(t)
	p, _ := s.CreatePlayer(ctx, "rook", "SHA256:aaa")

	started, err := s.StartRun(ctx, p.ID, "heist", 3)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	loaded, err := s.Run(ctx, p.ID, "heist")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if loaded.Salt != started.Salt {
		t.Fatal("salt did not survive a round trip")
	}
	if loaded.GameVersion != 3 {
		t.Fatalf("game version = %d, want 3 (pinned at start)", loaded.GameVersion)
	}

	// And the derived credentials match, which is what actually matters.
	if started.Deriver().Password("sysadmin") != loaded.Deriver().Password("sysadmin") {
		t.Fatal("reloaded run derives different passwords")
	}
}

func TestRunsAreUniquePerPlayerAndGame(t *testing.T) {
	s, ctx := open(t)
	p, _ := s.CreatePlayer(ctx, "rook", "SHA256:aaa")

	if _, err := s.StartRun(ctx, p.ID, "heist", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartRun(ctx, p.ID, "heist", 1); err == nil {
		t.Fatal("expected a second run of the same game to be rejected; use ResetRun")
	}
	// A different game is a different run.
	if _, err := s.StartRun(ctx, p.ID, "vault", 1); err != nil {
		t.Fatalf("second game: %v", err)
	}
}

func TestResetRerollsSecretsAndClearsProgress(t *testing.T) {
	s, ctx := open(t)
	p, _ := s.CreatePlayer(ctx, "rook", "SHA256:aaa")
	run, _ := s.StartRun(ctx, p.ID, "heist", 1)

	for _, id := range []string{"mailroom", "backup-op"} {
		if err := s.RecordProgress(ctx, run.ID, id); err != nil {
			t.Fatal(err)
		}
	}

	before := run.Deriver().Password("sysadmin")
	if err := s.ResetRun(ctx, run.ID, 2); err != nil {
		t.Fatalf("ResetRun: %v", err)
	}

	after, err := s.Run(ctx, p.ID, "heist")
	if err != nil {
		t.Fatal(err)
	}
	if after.Deriver().Password("sysadmin") == before {
		t.Fatal("reset left the credentials unchanged; a spoiled run stays spoiled")
	}
	if after.GameVersion != 2 {
		t.Fatalf("reset should adopt the current version, got %d", after.GameVersion)
	}

	progress, err := s.Progress(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 0 {
		t.Fatalf("reset left %d progress rows", len(progress))
	}
}

func TestProgressIsIdempotent(t *testing.T) {
	s, ctx := open(t)
	p, _ := s.CreatePlayer(ctx, "rook", "SHA256:aaa")
	run, _ := s.StartRun(ctx, p.ID, "heist", 1)

	for i := 0; i < 3; i++ {
		if err := s.RecordProgress(ctx, run.ID, "mailroom"); err != nil {
			t.Fatalf("RecordProgress: %v", err)
		}
	}

	progress, err := s.Progress(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 1 {
		t.Fatalf("passing through a level 3 times recorded %d rows", len(progress))
	}

	reached, err := s.HasReached(ctx, run.ID, "mailroom")
	if err != nil || !reached {
		t.Fatalf("HasReached(mailroom) = %v (%v)", reached, err)
	}
	if reached, _ := s.HasReached(ctx, run.ID, "sysadmin"); reached {
		t.Fatal("HasReached reported an unvisited level")
	}
}

func TestStickyHostIsSetAndCleared(t *testing.T) {
	s, ctx := open(t)
	p, _ := s.CreatePlayer(ctx, "rook", "SHA256:aaa")
	run, _ := s.StartRun(ctx, p.ID, "heist", 1)

	if err := s.SetHost(ctx, run.ID, "node-3"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Run(ctx, p.ID, "heist")
	if got.CurrentHost != "node-3" {
		t.Fatalf("CurrentHost = %q, want node-3", got.CurrentHost)
	}

	// The reaper clears it, freeing the run to be scheduled anywhere.
	if err := s.SetHost(ctx, run.ID, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Run(ctx, p.ID, "heist")
	if got.CurrentHost != "" {
		t.Fatalf("CurrentHost = %q after clearing", got.CurrentHost)
	}
}
