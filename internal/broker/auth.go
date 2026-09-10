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
	g, ok := a.cfg.Games.Game(conn.User())
	if !ok {
		return ""
	}
	return fmt.Sprintf("\r\n%s\r\n\r\n", g.Title)
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

	game, ok := a.cfg.Games.Game(requested)
	if !ok {
		// Deliberately terse: enumerating the games on offer is not something
		// an unauthenticated stranger needs to be able to do.
		return nil, fmt.Errorf("no such game")
	}
	if !known {
		// No enrollment here. The banner has already told them where to go.
		return nil, fmt.Errorf("permission denied")
	}

	run, err := a.cfg.Store.Run(ctx, player.ID, game.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("no run of this game; connect as %s to start one", EnrollUser)
	}
	if err != nil {
		a.log.Error("look up run", "error", err)
		return nil, fmt.Errorf("authentication unavailable")
	}

	a.beginSecondStage()

	// A run is in progress, so the second credential decides which level they
	// land on. Both kinds are offered because a game may gate a level either
	// way: a password found in a mailbox, or a private key recovered from a
	// backup and decrypted with a passphrase found somewhere else.
	return nil, &ssh.PartialSuccessError{
		Next: ssh.ServerAuthCallbacks{
			PasswordCallback:  a.levelPassword(fingerprint, player, game, run),
			PublicKeyCallback: a.levelKey(fingerprint, player, game, run),
		},
	}
}

// levelKey accepts a level's own private key as the credential for that level.
//
// This is what makes a split credential a real gate rather than a decoration:
// the key is recovered on one level and its passphrase on another, and neither
// is any use alone. The passphrase never reaches the server -- the client
// decrypts the key locally and proves possession by signature -- which is
// exactly how it would work against a real host.
func (a *authenticator) levelKey(
	fingerprint string, player *store.Player, game *manifest.Game, run *store.Run,
) func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
	return func(conn ssh.ConnMetadata, offered ssh.PublicKey) (*ssh.Permissions, error) {
		d := run.Deriver()
		marshalled := offered.Marshal()

		external := game.ExternalHosts()

		for _, l := range game.Levels {
			// Only levels the game actually opens with a key. Every level has a
			// derived key, but one the game never places is not a credential.
			if !game.ReceivesKey(l.ID) || !external[l.Host] {
				continue
			}

			pub, err := ssh.NewPublicKey(d.SSHKey(l.ID).Public())
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
				player: player, game: game, run: run,
				gameID: game.ID, levelID: l.ID,
			})
			return &ssh.Permissions{}, nil
		}

		return nil, fmt.Errorf("permission denied")
	}
}

// levelPassword turns the password prompt into a level selection. Any level's
// password is accepted, including one the player has not reached before: having
// the credential *is* having solved the level, which is the whole premise.
func (a *authenticator) levelPassword(
	fingerprint string, player *store.Player, game *manifest.Game, run *store.Run,
) func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
	return func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		// Only levels on machines the front door can see. A credential for an
		// internal machine is not refused because it is wrong -- it is refused
		// because that machine is not on the internet, and reaching it is the
		// puzzle.
		levelID, ok := run.Deriver().Match(game.ExternalLevelIDs(), string(password))
		if !ok {
			a.log.Info("level password rejected",
				"handle", player.Handle, "game", game.ID,
				"remote", conn.RemoteAddr().String())
			return nil, fmt.Errorf("permission denied")
		}

		a.set(&outcome{
			kind: outcomePlay, fingerprint: fingerprint,
			player: player, game: game, run: run,
			gameID: game.ID, levelID: levelID,
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
