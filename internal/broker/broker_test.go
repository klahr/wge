package broker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
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
	mu        sync.Mutex
	attached  []string // level ids, in order
	lastTerm  string
	lastSize  WindowSize
	lastCmd   string
	destroyed []int64
	refuse    error
}

// DestroyRun makes the fake a Resetter.
func (f *fakeRuntime) DestroyRun(_ context.Context, runID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyed = append(f.destroyed, runID)
	return nil
}

func (f *fakeRuntime) destroyedRuns() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64{}, f.destroyed...)
}

func (f *fakeRuntime) Attach(_ context.Context, s *Session) (int, error) {
	f.mu.Lock()
	if f.refuse != nil {
		err := f.refuse
		f.mu.Unlock()
		return 0, err
	}
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

// syncBuffer collects a session's output.
//
// x/crypto/ssh copies stdout and stderr in two goroutines, so a plain
// strings.Builder shared between them is a data race -- latent for as long as
// only one stream carried anything.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
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
	srv     *Server
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
	// Most tests are about what happens after somebody is a player, so they
	// take the open door. The invitation tests ask for the closed one.
	return newHarnessMode(t, game, true)
}

func newHarnessMode(t *testing.T, game *manifest.Game, open bool) *harness {
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
		Addr:           "127.0.0.1:0",
		HostKey:        newSigner(t),
		Store:          st,
		Games:          gameSet{game.ID: game},
		Runtime:        rt,
		Logger:         slog.New(slog.DiscardHandler),
		OpenEnrollment: open,
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

	return &harness{srv: srv, addr: addr.String(), store: st, runtime: rt, game: game}
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
	out := h.enrollDialog(t, key, handle)

	password := extractPassword(t, out)
	if password == "" {
		t.Fatalf("enrollment did not hand over a password; output:\n%s", out)
	}
	return password
}

// enrollDialog walks the enrolment entrance, answering each prompt in turn,
// and returns everything the server said.
func (h *harness) enrollDialog(t *testing.T, key ssh.Signer, answers ...string) string {
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
	var out syncBuffer
	sess.Stdout = &out
	sess.Stderr = &out

	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	for _, answer := range answers {
		fmt.Fprintf(stdin, "%s\r", answer)
	}
	_ = sess.Wait()

	return out.String()
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

// enrollAgain reconnects to the enrollment entrance as a known player and
// answers the reset question with the given line.
func (h *harness) enrollAgain(t *testing.T, key ssh.Signer, answer string) string {
	t.Helper()

	client, err := ssh.Dial("tcp", h.addr, &ssh.ClientConfig{
		User:            EnrollUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial enrollment: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	sess.Stdout = &out
	sess.Stderr = &out

	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(stdin, "%s\r", answer)
	_ = sess.Wait()

	return out.String()
}

// A reset is the remedy the derivation was designed around: the same puzzles
// come back with different answers, and no image is rebuilt.
func TestResetGivesTheSameGameNewAnswers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	key := newSigner(t)

	before := h.enroll(t, key, "rook")

	player, _ := h.store.PlayerByKey(ctx, ssh.FingerprintSHA256(key.PublicKey()))
	run, _ := h.store.Run(ctx, player.ID, h.game.ID)
	if err := h.store.RecordProgress(ctx, run.ID, "mailroom"); err != nil {
		t.Fatal(err)
	}

	out := h.enrollAgain(t, key, "reset")
	after := extractPassword(t, out)
	if after == "" {
		t.Fatalf("reset did not hand over a new password; output:\n%s", out)
	}

	if after == before {
		t.Fatal("the credentials did not change; a spoiled run stays spoiled")
	}

	// The machines carrying the old credentials must be gone.
	if got := h.runtime.destroyedRuns(); len(got) != 1 || got[0] != run.ID {
		t.Fatalf("destroyed runs = %v, want [%d]", got, run.ID)
	}

	progress, err := h.store.Progress(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 0 {
		t.Fatalf("reset left %d progress rows", len(progress))
	}

	// And the run is the same run, not a second one.
	fresh, err := h.store.Run(ctx, player.ID, h.game.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID != run.ID {
		t.Fatalf("reset created run %d instead of re-rolling %d", fresh.ID, run.ID)
	}
}

// Anything other than the word leaves the game alone. A menu number would be
// too easy to press by accident for something irreversible.
func TestEnrollmentWithoutTheWordDoesNotReset(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	key := newSigner(t)

	before := h.enroll(t, key, "rook")
	player, _ := h.store.PlayerByKey(ctx, ssh.FingerprintSHA256(key.PublicKey()))
	run, _ := h.store.Run(ctx, player.ID, h.game.ID)
	if err := h.store.RecordProgress(ctx, run.ID, "mailroom"); err != nil {
		t.Fatal(err)
	}

	for _, answer := range []string{"", "y", "yes", "RESET please", "1"} {
		out := h.enrollAgain(t, key, answer)
		if got := extractPassword(t, out); got != before {
			t.Fatalf("answering %q changed the password", answer)
		}
	}

	if got := h.runtime.destroyedRuns(); len(got) != 0 {
		t.Fatalf("machines were destroyed without the word being typed: %v", got)
	}
	progress, _ := h.store.Progress(ctx, run.ID)
	if len(progress) != 1 {
		t.Fatalf("progress was disturbed: %v", progress)
	}
}

// A player turned away with nothing to go on cannot tell a full host from a
// game they have broken, and will spend the evening looking for the mistake
// they did not make.
func TestCapacityRefusalSaysSo(t *testing.T) {
	h := newHarness(t)
	key := newSigner(t)
	password := h.enroll(t, key, "rook")

	h.runtime.mu.Lock()
	h.runtime.refuse = fmt.Errorf("no room: %w", ErrAtCapacity)
	h.runtime.mu.Unlock()

	client, err := h.dial(t, key, ssh.Password(password))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	var out syncBuffer
	sess.Stdout = &out
	sess.Stderr = &out
	runErr := sess.Run("")

	if !strings.Contains(out.String(), "at capacity") {
		t.Fatalf("the player was not told why; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "untouched") {
		t.Errorf("the player was not reassured their game survives:\n%s", out.String())
	}

	// EX_TEMPFAIL, so anything scripted can tell a wait from a failure.
	var exit *ssh.ExitError
	if !errors.As(runErr, &exit) {
		t.Fatalf("session ended with %v, want an exit status", runErr)
	}
	if exit.ExitStatus() != exitTempFail {
		t.Errorf("exit status = %d, want %d (EX_TEMPFAIL)", exit.ExitStatus(), exitTempFail)
	}
}

// A server nobody has to be invited to is a server anybody can fill.
func TestEnrolmentRequiresAnInvitation(t *testing.T) {
	h := newHarnessMode(t, testGame(), false)
	ctx := context.Background()
	key := newSigner(t)

	out := h.enrollDialog(t, key, "NOPE-NOPE-NOPE-NOPE", "NOPE-NOPE-NOPE-NOPE", "NOPE-NOPE-NOPE-NOPE")

	if !strings.Contains(out, "invitation only") {
		t.Fatalf("the server did not say it was closed:\n%s", out)
	}
	if !strings.Contains(out, "not a code I recognise") {
		t.Errorf("a wrong code was not explained:\n%s", out)
	}
	if extractPassword(t, out) != "" {
		t.Fatal("a game was started without an invitation")
	}
	if _, err := h.store.PlayerByKey(ctx, ssh.FingerprintSHA256(key.PublicKey())); err == nil {
		t.Fatal("a player was created without an invitation")
	}
}

func TestEnrolmentWithAnInvitation(t *testing.T) {
	h := newHarnessMode(t, testGame(), false)
	ctx := context.Background()
	key := newSigner(t)

	token, err := store.NewInviteToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateInvite(ctx, token, 1, time.Time{}, "test"); err != nil {
		t.Fatal(err)
	}

	out := h.enrollDialog(t, key, token, "rook")
	if extractPassword(t, out) == "" {
		t.Fatalf("an invited player was not let in:\n%s", out)
	}

	player, err := h.store.PlayerByKey(ctx, ssh.FingerprintSHA256(key.PublicKey()))
	if err != nil {
		t.Fatalf("the invited player was not created: %v", err)
	}
	if player.Handle != "rook" {
		t.Errorf("handle = %q", player.Handle)
	}

	// And the invitation is spent.
	if err := h.store.CheckInvite(ctx, token); !errors.Is(err, store.ErrInviteSpent) {
		t.Fatalf("after enrolment the invitation is %v, want spent", err)
	}
}

// The reason a code failed is worth saying: somebody hunting a typo that is
// not there gives up on the game rather than on the code.
func TestSpentAndExpiredCodesAreExplained(t *testing.T) {
	h := newHarnessMode(t, testGame(), false)
	ctx := context.Background()

	spent, _ := store.NewInviteToken()
	invite, err := h.store.CreateInvite(ctx, spent, 1, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.RevokeInvite(ctx, invite.ID); err != nil {
		t.Fatal(err)
	}

	expired, _ := store.NewInviteToken()
	if _, err := h.store.CreateInvite(ctx, expired, 1, time.Now().Add(-time.Hour), ""); err != nil {
		t.Fatal(err)
	}

	if out := h.enrollDialog(t, newSigner(t), spent, spent, spent); !strings.Contains(out, "already been used") {
		t.Errorf("a spent code was not explained:\n%s", out)
	}
	if out := h.enrollDialog(t, newSigner(t), expired, expired, expired); !strings.Contains(out, "has expired") {
		t.Errorf("an expired code was not explained:\n%s", out)
	}
}

// An open server asks for no code at all.
func TestOpenEnrolmentAsksForNoInvitation(t *testing.T) {
	h := newHarnessMode(t, testGame(), true)
	out := h.enrollDialog(t, newSigner(t), "rook")

	if strings.Contains(out, "invitation") {
		t.Fatalf("an open server asked for an invitation:\n%s", out)
	}
	if extractPassword(t, out) == "" {
		t.Fatalf("an open server did not enrol anybody:\n%s", out)
	}
}

// The limit is counted before anything is asked, so a script working through
// invitation codes is stopped by the same bound as one creating players.
func TestEnrolmentIsRateLimited(t *testing.T) {
	h := newHarnessMode(t, testGame(), true)
	h.srv.limiter = newEnrollLimiter(2, time.Hour)

	for i := 0; i < 2; i++ {
		out := h.enrollDialog(t, newSigner(t), fmt.Sprintf("player%d", i))
		if extractPassword(t, out) == "" {
			t.Fatalf("attempt %d was refused inside the limit:\n%s", i+1, out)
		}
	}

	out := h.enrollDialog(t, newSigner(t), "onetoomany")
	if !strings.Contains(out, "too many enrolment attempts") {
		t.Fatalf("the third attempt was not limited:\n%s", out)
	}
	if extractPassword(t, out) != "" {
		t.Fatal("a limited attempt still enrolled somebody")
	}
}
