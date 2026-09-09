package broker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klahr/wge/internal/manifest"
	"github.com/klahr/wge/internal/store"

	"golang.org/x/crypto/ssh"
)

// fakeRuntime stands in for Docker. It records what the broker asked for and
// prints a line the test can recognise.
type fakeRuntime struct {
	mu       sync.Mutex
	attached []string // level ids, in order
	lastTerm string
	lastSize WindowSize
	lastCmd  string
}

func (f *fakeRuntime) Attach(_ context.Context, s *Session) (int, error) {
	f.mu.Lock()
	f.attached = append(f.attached, s.Level.ID)
	f.lastTerm = s.Term
	f.lastSize = WindowSize{Width: s.Width, Height: s.Height}
	f.lastCmd = s.Command
	f.mu.Unlock()

	fmt.Fprintf(s.Stdout, "attached:%s:%s\r\n", s.Level.ID, s.Level.User)
	return 0, nil
}

func (f *fakeRuntime) levels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.attached...)
}

type gameSet map[string]*manifest.Game

func (g gameSet) Game(id string) (*manifest.Game, bool) {
	game, ok := g[id]
	return game, ok
}

func (g gameSet) All() []*manifest.Game {
	out := make([]*manifest.Game, 0, len(g))
	for _, game := range g {
		out = append(out, game)
	}
	return out
}

func testGame() *manifest.Game {
	g, err := manifest.Load(filepath.Join("..", "..", "games", "heist"))
	if err != nil {
		panic(err)
	}
	return g
}

type harness struct {
	addr    string
	store   *store.Store
	runtime *fakeRuntime
	game    *manifest.Game
}

// keyGame is a minimal single-host game whose second level is opened by an SSH
// key. The reference game's key-opened level lives on an internal machine and
// is deliberately unreachable from the front door, so the key tests need a game
// whose perimeter is not the thing under test.
func keyGame() *manifest.Game {
	return &manifest.Game{
		ID: "keygame", Version: 1, Title: "Key Fixture",
		Base: "wge/debian-13",
		Levels: []*manifest.Level{
			{
				ID: "front", User: "alpha", Host: manifest.DefaultHostID,
				Grants: []manifest.Grant{{
					Kind: manifest.GrantSSHKey, To: "back",
					PlacedIn: "/home/alpha/id_ed25519",
				}},
			},
			{ID: "back", User: "beta", Host: manifest.DefaultHostID, Requires: []string{"front"}},
		},
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, testGame())
}

func newHarnessWith(t *testing.T, game *manifest.Game) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "wge.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	rt := &fakeRuntime{}

	srv, err := New(Config{
		Addr:    "127.0.0.1:0",
		HostKey: newSigner(t),
		Store:   st,
		Games:   gameSet{game.ID: game},
		Runtime: rt,
		Logger:  slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("new broker: %v", err)
	}

	addr, err := srv.Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ctx)
	t.Cleanup(func() { srv.Close() })

	return &harness{addr: addr.String(), store: st, runtime: rt, game: game}
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return signer
}

func (h *harness) dial(t *testing.T, key ssh.Signer, methods ...ssh.AuthMethod) (*ssh.Client, error) {
	t.Helper()
	auth := append([]ssh.AuthMethod{ssh.PublicKeys(key)}, methods...)
	return ssh.Dial("tcp", h.addr, &ssh.ClientConfig{
		User:            h.game.ID,
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
}

// enroll walks a fresh key through registration and returns the handle it chose
// along with the entry password it was given.
func (h *harness) enroll(t *testing.T, key ssh.Signer, handle string) string {
	t.Helper()

	client, err := ssh.Dial("tcp", h.addr, &ssh.ClientConfig{
		User:            EnrollUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial for enrollment: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()

	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	sess.Stdout = &out
	sess.Stderr = &out

	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	fmt.Fprintf(stdin, "%s\r", handle)
	if err := sess.Wait(); err != nil {
		t.Fatalf("enrollment session: %v (output: %q)", err, out.String())
	}

	password := extractPassword(t, out.String())
	if password == "" {
		t.Fatalf("enrollment did not hand over a password; output:\n%s", out.String())
	}
	return password
}

// extractPassword picks the indented credential out of the enrollment text.
func extractPassword(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if len(trimmed) == 24 && strings.HasPrefix(line, "    ") {
			return trimmed
		}
	}
	return ""
}

// The whole flow: a new key enrolls, is handed the entry password, and that
// password is what logs them in.
func TestEnrollThenPlay(t *testing.T) {
	h := newHarness(t)
	key := newSigner(t)

	password := h.enroll(t, key, "rook")

	client, err := h.dial(t, key, ssh.Password(password))
	if err != nil {
		t.Fatalf("dial with entry password: %v", err)
	}
	defer client.Close()

	out := runShell(t, client)
	if !strings.Contains(out, "attached:mailroom:jposti") {
		t.Fatalf("expected to land on the entry level, got:\n%s", out)
	}
	if got := h.runtime.levels(); len(got) != 1 || got[0] != "mailroom" {
		t.Fatalf("runtime attachments = %v", got)
	}
}

// The resume property, end to end: the password from one day opens the game on
// the next, against a broker that has kept nothing but the salt.
func TestPasswordStillWorksOnAReconnect(t *testing.T) {
	h := newHarness(t)
	key := newSigner(t)
	password := h.enroll(t, key, "rook")

	for i := 0; i < 3; i++ {
		client, err := h.dial(t, key, ssh.Password(password))
		if err != nil {
			t.Fatalf("reconnect %d: %v", i, err)
		}
		out := runShell(t, client)
		client.Close()
		if !strings.Contains(out, "attached:mailroom") {
			t.Fatalf("reconnect %d landed wrong:\n%s", i, out)
		}
	}
}

// Holding a level's credential is what grants access to it -- a player who
// finds a later password may use it, having earned it.
func TestAnyHeldCredentialSelectsItsLevel(t *testing.T) {
	h := newHarness(t)
	key := newSigner(t)
	h.enroll(t, key, "rook")

	player, err := h.store.PlayerByKey(context.Background(), ssh.FingerprintSHA256(key.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	run, err := h.store.Run(context.Background(), player.ID, h.game.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Jump past the entry level using a credential for a later one.
	client, err := h.dial(t, key, ssh.Password(run.Deriver().Password("ops-oncall")))
	if err != nil {
		t.Fatalf("dial with a later level's password: %v", err)
	}
	defer client.Close()

	if out := runShell(t, client); !strings.Contains(out, "attached:ops-oncall:oncall") {
		t.Fatalf("expected the ops-oncall level, got:\n%s", out)
	}
}

// A machine that is not on the internet is not reachable from the front door,
// however good the credential. Reaching it is the puzzle.
func TestInternalLevelIsNotReachableFromTheFrontDoor(t *testing.T) {
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

	if _, err := h.dial(t, key, ssh.Password(run.Deriver().Password("sysadmin"))); err == nil {
		t.Fatal("the front door attached a player to a machine that is not on the internet")
	}
}

// The anti-walkthrough property, end to end.
func TestAnotherPlayersPasswordIsRejected(t *testing.T) {
	h := newHarness(t)

	keyA, keyB := newSigner(t), newSigner(t)
	passwordA := h.enroll(t, keyA, "rook")
	h.enroll(t, keyB, "vega")

	// B publishes A's password. It opens nothing for B.
	if _, err := h.dial(t, keyB, ssh.Password(passwordA)); err == nil {
		t.Fatal("another player's password was accepted")
	}
}

func TestWrongPasswordIsRejected(t *testing.T) {
	h := newHarness(t)
	key := newSigner(t)
	h.enroll(t, key, "rook")

	if _, err := h.dial(t, key, ssh.Password("definitely-not-the-password")); err == nil {
		t.Fatal("a wrong password authenticated")
	}
	if got := h.runtime.levels(); len(got) != 0 {
		t.Fatalf("a failed login still reached the runtime: %v", got)
	}
}

// A key alone must not be enough once a run exists, or the credential chain --
// the entire game -- would be bypassable.
func TestKeyAloneCannotEnterAnActiveRun(t *testing.T) {
	h := newHarness(t)
	key := newSigner(t)
	h.enroll(t, key, "rook")

	if _, err := h.dial(t, key); err == nil {
		t.Fatal("public key alone opened a run that already exists")
	}
}

// A player using a credential they found offers a key the engine has never
// seen. That must be refused, not answered with a registration prompt that
// would start them a second run of the same game.
func TestUnknownKeyCannotEnrollThroughAGame(t *testing.T) {
	h := newHarness(t)

	if _, err := h.dial(t, newSigner(t)); err == nil {
		t.Fatal("an unrecognised key was let into a game; enrollment has its own entrance")
	}
}

// A level opened by an SSH key is entered by proving possession of that key.
// The passphrase never reaches the server: the client decrypts locally.
func TestLevelKeyOpensItsLevel(t *testing.T) {
	h := newHarnessWith(t, keyGame())
	ctx := context.Background()
	playerKey := newSigner(t)
	h.enroll(t, playerKey, "rook")

	player, err := h.store.PlayerByKey(ctx, ssh.FingerprintSHA256(playerKey.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	run, err := h.store.Run(ctx, player.ID, h.game.ID)
	if err != nil {
		t.Fatal(err)
	}

	levelKey, err := ssh.NewSignerFromKey(run.Deriver().SSHKey("back"))
	if err != nil {
		t.Fatal(err)
	}

	// Both identities go in one publickey method, which is what an OpenSSH
	// client does with two -i flags: it offers them in turn, and the level key
	// is the one the second stage accepts.
	client, err := ssh.Dial("tcp", h.addr, &ssh.ClientConfig{
		User:            h.game.ID,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(playerKey, levelKey)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial with the level key: %v", err)
	}
	defer client.Close()

	if out := runShell(t, client); !strings.Contains(out, "attached:back:beta") {
		t.Fatalf("expected the key to open its level, got:\n%s", out)
	}
}

// Another run's key must not open this one, exactly as another run's password
// must not.
func TestAnotherRunsLevelKeyIsRejected(t *testing.T) {
	h := newHarnessWith(t, keyGame())
	ctx := context.Background()

	keyA, keyB := newSigner(t), newSigner(t)
	h.enroll(t, keyA, "rook")
	h.enroll(t, keyB, "vega")

	playerA, _ := h.store.PlayerByKey(ctx, ssh.FingerprintSHA256(keyA.PublicKey()))
	runA, _ := h.store.Run(ctx, playerA.ID, h.game.ID)
	levelKeyA, err := ssh.NewSignerFromKey(runA.Deriver().SSHKey("back"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = ssh.Dial("tcp", h.addr, &ssh.ClientConfig{
		User:            h.game.ID,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(keyB, levelKeyA)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err == nil {
		t.Fatal("another run's level key was accepted")
	}
}

func TestUnknownGameIsRefused(t *testing.T) {
	h := newHarness(t)
	_, err := ssh.Dial("tcp", h.addr, &ssh.ClientConfig{
		User:            "no-such-game",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(newSigner(t))},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err == nil {
		t.Fatal("connected to a game that does not exist")
	}
}

func TestProgressIsRecordedOnArrival(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	key := newSigner(t)
	password := h.enroll(t, key, "rook")

	client, err := h.dial(t, key, ssh.Password(password))
	if err != nil {
		t.Fatal(err)
	}
	runShell(t, client)
	client.Close()

	player, _ := h.store.PlayerByKey(ctx, ssh.FingerprintSHA256(key.PublicKey()))
	run, _ := h.store.Run(ctx, player.ID, h.game.ID)

	progress, err := h.store.Progress(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 1 || progress[0].LevelID != "mailroom" {
		t.Fatalf("progress = %+v", progress)
	}
}

// A non-interactive command's output must be only that command's output.
// Prepending the message of the day breaks anything scripted over ssh.
func TestMOTDIsNotPrependedToCommandOutput(t *testing.T) {
	h := newHarness(t)
	key := newSigner(t)
	password := h.enroll(t, key, "rook")

	client, err := h.dial(t, key, ssh.Password(password))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	out, err := sess.Output("id -un")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	motd := strings.TrimSpace(h.game.Levels[0].Narrative.MOTD)
	if motd == "" {
		t.Skip("the reference game's entry level has no MOTD to leak")
	}
	if strings.Contains(string(out), strings.Split(motd, "\n")[0]) {
		t.Fatalf("the message of the day leaked into command output:\n%s", out)
	}
}

func TestPtyAndWindowSizeReachTheRuntime(t *testing.T) {
	h := newHarness(t)
	key := newSigner(t)
	password := h.enroll(t, key, "rook")

	client, err := h.dial(t, key, ssh.Password(password))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if err := sess.RequestPty("screen-256color", 40, 132, ssh.TerminalModes{}); err != nil {
		t.Fatalf("pty: %v", err)
	}
	var out strings.Builder
	sess.Stdout = &out
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	_ = sess.Wait()

	h.runtime.mu.Lock()
	defer h.runtime.mu.Unlock()
	if h.runtime.lastTerm != "screen-256color" {
		t.Errorf("TERM = %q", h.runtime.lastTerm)
	}
	if h.runtime.lastSize != (WindowSize{Width: 132, Height: 40}) {
		t.Errorf("window size = %+v", h.runtime.lastSize)
	}
}

// The broker must not be usable as a proxy: a player with a shell on a game box
// could otherwise tunnel straight past the run's own egress rules.
func TestPortForwardingIsRefused(t *testing.T) {
	h := newHarness(t)
	key := newSigner(t)
	password := h.enroll(t, key, "rook")

	client, err := h.dial(t, key, ssh.Password(password))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if _, err := client.Dial("tcp", "example.com:80"); err == nil {
		t.Fatal("the broker forwarded a TCP connection")
	}
}

func runShell(t *testing.T, client *ssh.Client) string {
	t.Helper()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()

	out, err := sess.Output("")
	if err != nil && !strings.Contains(err.Error(), "exited") {
		// A shell that exits 0 is normal here; anything else is worth seeing.
		if _, ok := err.(*ssh.ExitError); !ok && err != io.EOF {
			t.Fatalf("session: %v", err)
		}
	}
	return string(out)
}
