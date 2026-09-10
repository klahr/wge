package broker

import (
	"log/slog"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// stubMeta is the little of ssh.ConnMetadata a banner reads.
type stubMeta struct {
	user string
	ssh.ConnMetadata
}

func (s stubMeta) User() string { return s.user }

func bannerFor(user string) string {
	game := testGame()
	a := newAuthenticator(
		Config{Games: gameSet{game.ID: game}},
		slog.New(slog.DiscardHandler),
	)
	return a.banner(stubMeta{user: user})
}

func TestBannerGreetsAGameWithItsTitle(t *testing.T) {
	got := bannerFor(testGame().ID)
	if !strings.Contains(got, testGame().Title) {
		t.Fatalf("banner for a game does not name it: %q", got)
	}
}

// A username that is neither a game nor an account gets a bare "permission
// denied (publickey)" from ssh, which is indistinguishable from a broken key.
func TestBannerTellsAnUnknownNameWhereCredentialsGo(t *testing.T) {
	got := bannerFor("nobody")
	for _, want := range []string{"nobody", "password", EnrollUser} {
		if !strings.Contains(got, want) {
			t.Fatalf("banner does not mention %q: %q", want, got)
		}
	}
}

// The terseness elsewhere in the auth chain is deliberate: a stranger may
// learn that the name they tried is nothing here, never what the games are.
func TestBannerNamesNoGameItWasNotAsked(t *testing.T) {
	game := testGame()
	got := bannerFor("nobody")
	if strings.Contains(got, game.ID) || strings.Contains(got, game.Title) {
		t.Fatalf("banner enumerates the library: %q", got)
	}
}

// A level's account is a login like any other, so it is neither scolded nor
// told which game it belongs to.
func TestBannerKeepsALevelAccountsGameToItself(t *testing.T) {
	game := testGame()
	account := game.Levels[0].User

	got := bannerFor(account)
	if strings.Contains(got, "nothing called") {
		t.Fatalf("account %q was told it does not exist: %q", account, got)
	}
	if strings.Contains(got, game.ID) || strings.Contains(got, game.Title) {
		t.Fatalf("banner gives away the game behind %q: %q", account, got)
	}
}
