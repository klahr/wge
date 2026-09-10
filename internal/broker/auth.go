package broker

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/klahr/wge/internal/manifest"
	"github.com/klahr/wge/internal/store"

	"golang.org/x/crypto/ssh"
)

// maxAuthTries bounds password guessing per connection. Derived passwords have
// far too much entropy to guess, but an unbounded prompt is still a free
// amplifier for anyone with a botnet.
const maxAuthTries = 6

// EnrollUser is the SSH username that registers a player and starts a game.
//
// Enrollment needs its own entrance rather than being what happens when an
// unrecognised key connects to a game. A player using a credential they found
// -- `ssh -i recovered_key heist@host` -- is offering a key the engine has
// never seen, and OpenSSH does not necessarily offer their own identity first.
// Treating any unknown key as a newcomer would answer that with a registration
// prompt and, worse, a second run of the game with different answers.
const EnrollUser = "enroll"

// outcomeKind is what a successful authentication earned.
type outcomeKind int

const (
	// outcomePlay attaches the player to a level they hold the credential for.
	outcomePlay outcomeKind = iota
	// outcomeEnroll opens the only out-of-world dialog in the system: naming a
	// new player, or starting a run and handing over its first password. Once a
	// run exists there is nothing left to say out of character.
	outcomeEnroll
)

type outcome struct {
	kind        outcomeKind
	fingerprint string
	remote      string
	player      *store.Player // nil for a player who has never connected
	game        *manifest.Game
	run         *store.Run // nil until a run exists
	gameID      string
	levelID     string
}

// handle names the player for logging. A key that has never been seen has no
// player behind it yet, which is the normal state at the start of enrollment.
func (o *outcome) handle() string {
	if o.player == nil {
		return "<new>"
	}
	return o.player.Handle
}

// authenticator runs one connection's auth chain. A fresh one is built per
// connection, so it can hold that connection's result directly.
type authenticator struct {
	cfg Config
	log *slog.Logger

	mu sync.Mutex
	// result is what the completed chain decided.
	result *outcome
	// secondStage is set once the player's identity key has been accepted and
	// the connection has moved on to the credential that selects a level.
	secondStage bool
	// levelKeyFingerprint is the key the second stage accepted, so that the
	// verification pass can confirm it is the same one.
	levelKeyFingerprint string
}

func newAuthenticator(cfg Config, log *slog.Logger) *authenticator {
	return &authenticator{cfg: cfg, log: log}
}

func (a *authenticator) serverConfig() *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		MaxAuthTries: maxAuthTries,

		// Offering a key is enough to be asked for a password; the key is not
		// yet proven here, so nothing is decided.
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},

		// The key is proven by this point, so the player is known and the auth
		// chain can branch on who they are.
		VerifiedPublicKeyCallback: a.keyVerified,

		BannerCallback: a.banner,
	}
	cfg.AddHostKey(a.cfg.HostKey)
	return cfg
}

// banner greets the connection in character, before any credential is asked
// for -- exactly as a real host's pre-auth banner would.
func (a *authenticator) banner(conn ssh.ConnMetadata) string {
	if conn.User() == EnrollUser {
		return "\r\n"
	}
	if g, ok := a.cfg.Games.Game(conn.User()); ok {
		return fmt.Sprintf("\r\n%s\r\n\r\n", g.Title)
	}
	if a.namesLevelAccount(conn.User()) {
		// A level's account, which is a login like any other. Its game is not
		// named: which game an account belongs to is part of what a player
		// works out, and the banner is sent before anybody has proven a key.
		return "\r\n"
	}

	// A username that is neither. On its own the rejection that follows is a
	// bare "permission denied (publickey)", which reads as a broken key -- the
	// one thing that is not wrong. Saying where the two halves of a credential
	// go costs nothing here, because it names no game and no account.
	return fmt.Sprintf("\r\nThere is nothing called %q on this server.\r\n"+
		"Log in as the account whose password you found, or as the game:\r\n"+
		"either way, the password is what chooses the level.\r\n"+
		"\r\n    ssh <account>@<this host>\r\n"+
		"    ssh <game>@<this host>\r\n"+
		"\r\nNobody here yet? Connect as %s.\r\n\r\n", conn.User(), EnrollUser)
}

// namesLevelAccount reports whether a username is a level's account in any
// game served. It answers the banner, which runs before a key is proven and so
// cannot know whose runs to look in -- unlike the auth chain, which considers
// only the player's own.
func (a *authenticator) namesLevelAccount(user string) bool {
	for _, g := range a.cfg.Games.All() {
		external := g.ExternalHosts()
		for _, l := range g.Levels {
			if l.User == user && external[l.Host] {
				return true
			}
		}
	}
	return false
}

// keyVerified resolves the player and decides whether a password is still
// needed. It returns a partial success -- rather than authenticating outright
// -- whenever the player has a run in progress, because for them the password
// prompt is not a formality but the move that selects a level.
func (a *authenticator) keyVerified(
	conn ssh.ConnMetadata, key ssh.PublicKey, perms *ssh.Permissions, _ string,
) (*ssh.Permissions, error) {
	ctx := context.Background()
	fingerprint := ssh.FingerprintSHA256(key)

	// VerifiedPublicKeyCallback is a property of the connection, not of one
	// authentication stage, so it runs again for a level key offered as the
	// second credential. That key belongs to a level, not to a player, and
	// looking it up as an identity would reject a login the second stage has
	// already accepted.
	if done, ok := a.verifiedSecondStage(fingerprint); ok {
		if !done {
			return nil, fmt.Errorf("permission denied")
		}
		return perms, nil
	}

	requested := conn.User()

	player, err := a.cfg.Store.PlayerByKey(ctx, fingerprint)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		a.log.Error("look up player by key", "error", err)
		return nil, fmt.Errorf("authentication unavailable")
	}
	known := err == nil

	// The enrollment entrance takes anyone: a key nobody has seen belongs to
	// somebody new, and a key we know belongs to a player starting another game.
	if requested == EnrollUser {
		a.set(&outcome{
			kind: outcomeEnroll, fingerprint: fingerprint,
			player: player, remote: conn.RemoteAddr().String(),
		})
		return perms, nil
	}

	if !known {
		// No enrollment here. The banner has already told them where to go.
		// This is also the answer to a username that names nothing at all:
		// enumerating the games on offer is not something an unauthenticated
		// stranger needs to be able to do.
		return nil, fmt.Errorf("permission denied")
	}

	// Two ways to name what you are logging in to, and both are legitimate.
	// A game lets any level's password select a level, which is how a player
	// with a newly found credential gets in without knowing whose account it
	// is. A level's account narrows it to that one level, which is how the
	// credential reads on the box it came from: the account name was part of
	// what the previous level yielded.
	var targets []levelTarget
	if game, isGame := a.cfg.Games.Game(requested); isGame {
		run, err := a.cfg.Store.Run(ctx, player.ID, game.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("no run of this game; connect as %s to start one", EnrollUser)
		}
		if err != nil {
			a.log.Error("look up run", "error", err)
			return nil, fmt.Errorf("authentication unavailable")
		}
		targets = runTargets(game, run)
	} else {
		var err error
		if targets, err = a.accountTargets(ctx, player, requested); err != nil {
			a.log.Error("look up runs", "error", err)
			return nil, fmt.Errorf("authentication unavailable")
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no game or reachable level account %q for this player", requested)
	}

	a.beginSecondStage()

	// The second credential decides which of the targets they land on. Both
	// kinds are offered because a game may gate a level either way: a password
	// found in a mailbox, or a private key recovered from a backup and
	// decrypted with a passphrase found somewhere else.
	return nil, &ssh.PartialSuccessError{
		Next: ssh.ServerAuthCallbacks{
			PasswordCallback:  a.levelPassword(fingerprint, player, targets),
			PublicKeyCallback: a.levelKey(fingerprint, player, targets),
		},
	}
}

// levelTarget is one level a second-stage credential may open: the level, the
// game it belongs to, and the run whose salt derives its credentials.
type levelTarget struct {
	game  *manifest.Game
	run   *store.Run
	level *manifest.Level
}

// runTargets is every level of a run the front door will attach a player to.
//
// Levels on internal machines are left out. A credential for one is not
// refused because it is wrong -- it is refused because that machine is not on
// the internet, and reaching it is the puzzle.
func runTargets(game *manifest.Game, run *store.Run) []levelTarget {
	external := game.ExternalHosts()

	var out []levelTarget
	for _, l := range game.Levels {
		if external[l.Host] {
			out = append(out, levelTarget{game: game, run: run, level: l})
		}
	}
	return out
}

// accountTargets finds the levels a login naming an account could mean.
//
// An account name is unique within a game, but nothing stops two games from
// employing a sysop, so every level of that name across the player's own runs
// is a candidate and the credential decides between them -- exactly as it
// decides between the levels of one game. Only the player's runs are
// considered: a level of a game they have never started is not theirs to log
// in to, and searching every run on the server would make one player's
// progress reachable with another player's key.
func (a *authenticator) accountTargets(
	ctx context.Context, player *store.Player, account string,
) ([]levelTarget, error) {
	runs, err := a.cfg.Store.PlayerRuns(ctx, player.ID)
	if err != nil {
		return nil, err
	}

	var out []levelTarget
	for _, run := range runs {
		game, ok := a.cfg.Games.Game(run.GameID)
		if !ok {
			// A run of a game this engine no longer serves.
			continue
		}
		for _, t := range runTargets(game, run) {
			if t.level.User == account {
				out = append(out, t)
			}
		}
	}
	return out, nil
}

// levelKey accepts a level's own private key as the credential for that level.
//
// This is what makes a split credential a real gate rather than a decoration:
// the key is recovered on one level and its passphrase on another, and neither
// is any use alone. The passphrase never reaches the server -- the client
// decrypts the key locally and proves possession by signature -- which is
// exactly how it would work against a real host.
func (a *authenticator) levelKey(
	fingerprint string, player *store.Player, targets []levelTarget,
) func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
	return func(conn ssh.ConnMetadata, offered ssh.PublicKey) (*ssh.Permissions, error) {
		marshalled := offered.Marshal()

		for _, t := range targets {
			// Only levels the game actually opens with a key. Every level has a
			// derived key, but one the game never places is not a credential.
			if !t.game.ReceivesKey(t.level.ID) {
				continue
			}

			pub, err := ssh.NewPublicKey(t.run.Deriver().SSHKey(t.level.ID).Public())
			if err != nil {
				continue
			}
			if subtle.ConstantTimeCompare(pub.Marshal(), marshalled) != 1 {
				continue
			}

			// The signature has not been checked yet; x/crypto verifies it
			// before this authentication is allowed to succeed, and calls back
			// once more with whichever key is finally used.
			a.acceptLevelKey(ssh.FingerprintSHA256(offered), &outcome{
				kind: outcomePlay, fingerprint: fingerprint,
				player: player, game: t.game, run: t.run,
				gameID: t.game.ID, levelID: t.level.ID,
			})
			return &ssh.Permissions{}, nil
		}

		return nil, fmt.Errorf("permission denied")
	}
}

// levelPassword turns the password prompt into a level selection. A level the
// player has not reached before is accepted like any other: having the
// credential *is* having solved the level, which is the whole premise.
//
// Every target is compared even after one matches, so the time this takes says
// nothing about which level a password belongs to.
func (a *authenticator) levelPassword(
	fingerprint string, player *store.Player, targets []levelTarget,
) func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
	return func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		var found *levelTarget
		for i, t := range targets {
			want := t.run.Deriver().Password(t.level.ID)
			if subtle.ConstantTimeCompare([]byte(want), password) == 1 {
				found = &targets[i]
			}
		}
		if found == nil {
			a.log.Info("level password rejected",
				"handle", player.Handle, "user", conn.User(),
				"remote", conn.RemoteAddr().String())
			return nil, fmt.Errorf("permission denied")
		}

		a.set(&outcome{
			kind: outcomePlay, fingerprint: fingerprint,
			player: player, game: found.game, run: found.run,
			gameID: found.game.ID, levelID: found.level.ID,
		})
		return &ssh.Permissions{}, nil
	}
}

func (a *authenticator) set(o *outcome) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.result = o
}

// beginSecondStage records that the player's identity is settled and the
// connection has moved on to the credential that picks a level.
func (a *authenticator) beginSecondStage() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.secondStage = true
}

// acceptLevelKey records the level key the second stage accepted, pending
// verification of its signature.
func (a *authenticator) acceptLevelKey(fingerprint string, o *outcome) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.levelKeyFingerprint = fingerprint
	a.result = o
}

// verifiedSecondStage reports whether a verified key belongs to the second
// stage, and if so whether it is the key that stage accepted.
func (a *authenticator) verifiedSecondStage(fingerprint string) (accepted, isSecondStage bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.secondStage {
		return false, false
	}
	return a.levelKeyFingerprint == fingerprint, true
}

// outcome returns what the completed auth chain decided.
func (a *authenticator) outcome(*ssh.Permissions) (*outcome, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.result == nil {
		return nil, errors.New("authentication completed without an outcome")
	}
	return a.result, nil
}
