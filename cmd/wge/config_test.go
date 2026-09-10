package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klahr/wge/internal/store"
)

// A command run from the wrong directory must fail rather than make itself an
// empty database: the invitation it would print is one no server accepts.
func TestOpenStoreRefusesToCreateADatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wge.db")

	_, err := openStore(context.Background(), path)
	if err == nil {
		t.Fatal("a missing database was created")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error does not say which path is missing: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the database was created anyway")
	}
}

func TestOpenStoreOpensADatabaseThatExists(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wge.db")

	made, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	made.Close()

	st, err := openStore(ctx, path)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	st.Close()
}
