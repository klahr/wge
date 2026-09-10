package library

import (
	"strings"
	"testing"

	"github.com/klahr/wge/internal/manifest"
)

func testLibrary() *Library {
	return &Library{games: map[string]*manifest.Game{
		"demo":  {ID: "demo"},
		"heist": {ID: "heist"},
	}}
}

func TestOnlyKeepsTheGamesNamed(t *testing.T) {
	lib, err := testLibrary().Only([]string{"demo"})
	if err != nil {
		t.Fatalf("Only: %v", err)
	}
	if got := lib.IDs(); len(got) != 1 || got[0] != "demo" {
		t.Fatalf("library holds %v, want [demo]", got)
	}
	if _, ok := lib.Game("heist"); ok {
		t.Fatal("a game that was not named is still served")
	}
}

// An unknown id fails the load rather than narrowing to nothing: a server that
// starts with no game answers no password anybody has.
func TestOnlyRejectsAGameTheLibraryDoesNotHold(t *testing.T) {
	_, err := testLibrary().Only([]string{"demo", "hiest"})
	if err == nil {
		t.Fatal("a misspelled game id was accepted")
	}
	if !strings.Contains(err.Error(), "demo, heist") {
		t.Fatalf("error names no alternatives: %v", err)
	}
}

func TestOnlyRejectsAnEmptySelection(t *testing.T) {
	if _, err := testLibrary().Only(nil); err == nil {
		t.Fatal("serving no game at all was accepted")
	}
}
