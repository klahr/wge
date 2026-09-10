package broker

import (
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// dialAs is dial(), for a username that is not the game.
func (h *harness) dialAs(t *testing.T, user string, key ssh.Signer, methods ...ssh.AuthMethod) (*ssh.Client, error) {
	t.Helper()
	return ssh.Dial("tcp", h.addr, &ssh.ClientConfig{
		User:            user,
		Auth:            append([]ssh.AuthMethod{ssh.PublicKeys(key)}, methods...),
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
}

// The credential a player finds names an account, so the account is a login:
// the password found on the previous level opens the level that owns it.
func TestALevelsAccountIsALogin(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	key := newSigner(t)
	h.enroll(t, key, "rook")

	player, err := h.store.PlayerByKey(ctx, ssh.FingerprintSHA256(key.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	run, err := h.store.Run(ctx, player.ID, h.game.ID)
	if err != nil {
		t.Fatal(err)
	}

	client, err := h.dialAs(t, "oncall", key, ssh.Password(run.Deriver().Password("ops-oncall")))
	if err != nil {
		t.Fatalf("dial as a level's account: %v", err)
	}
	defer client.Close()

	if out := runShell(t, client); !strings.Contains(out, "attached:ops-oncall:oncall") {
		t.Fatalf("expected the ops-oncall level, got:\n%s", out)
	}
}

// Naming the account narrows the login to it. Another level's password is a
// credential the player holds, but not for the account they asked for.
func TestAnAccountAcceptsOnlyItsOwnPassword(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	key := newSigner(t)
	h.enroll(t, key, "rook")

	player, _ := h.store.PlayerByKey(ctx, ssh.FingerprintSHA256(key.PublicKey()))
	run, _ := h.store.Run(ctx, player.ID, h.game.ID)

	if _, err := h.dialAs(t, "oncall", key, ssh.Password(run.Deriver().Password("mailroom"))); err == nil {
		t.Fatal("an account accepted a different level's password")
	}
}

// An account on a machine that is not on the internet is not a front door
// login either: reaching that machine is the puzzle.
func TestAnInternalAccountIsNotALogin(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	key := newSigner(t)
	h.enroll(t, key, "rook")

	player, _ := h.store.PlayerByKey(ctx, ssh.FingerprintSHA256(key.PublicKey()))
	run, _ := h.store.Run(ctx, player.ID, h.game.ID)

	sysadmin, ok := h.game.Level("sysadmin")
	if !ok {
		t.Fatal("the reference game has no sysadmin level")
	}
	if h.game.ExternalHosts()[sysadmin.Host] {
		t.Skipf("host %s is external; nothing to test", sysadmin.Host)
	}

	if _, err := h.dialAs(t, sysadmin.User, key, ssh.Password(run.Deriver().Password("sysadmin"))); err == nil {
		t.Fatal("the front door attached a player to a machine that is not on the internet")
	}
}

// The anti-walkthrough property holds on this route too: the account names are
// public, the passwords behind them are per run.
func TestAnotherPlayersPasswordIsRejectedByAccount(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	keyA, keyB := newSigner(t), newSigner(t)
	h.enroll(t, keyA, "rook")
	h.enroll(t, keyB, "pawn")

	playerA, _ := h.store.PlayerByKey(ctx, ssh.FingerprintSHA256(keyA.PublicKey()))
	runA, _ := h.store.Run(ctx, playerA.ID, h.game.ID)

	if _, err := h.dialAs(t, "oncall", keyB, ssh.Password(runA.Deriver().Password("ops-oncall"))); err == nil {
		t.Fatal("one player's password opened an account in another player's run")
	}
}

// A key nobody has seen gets nothing, whichever name it asks for.
func TestAnAccountIsNotAWayIn(t *testing.T) {
	h := newHarness(t)
	if _, err := h.dialAs(t, "oncall", newSigner(t), ssh.Password("anything")); err == nil {
		t.Fatal("an unenrolled key logged in as a level account")
	}
}
