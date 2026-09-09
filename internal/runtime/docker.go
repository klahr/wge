// Package runtime owns container lifecycle: creating the box for a run on
// demand, attaching a player to it, and tearing it down when they leave.
//
// Because a container is a pure function of (game_version, run_salt), it is
// disposable at any moment. Nothing here needs to preserve container state, and
// a box that is destroyed mid-session costs a player only their scrollback.
package runtime

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sync"

	"github.com/klahr/wge/internal/broker"
	"github.com/klahr/wge/internal/build"
	"github.com/klahr/wge/internal/docker"
	"github.com/klahr/wge/internal/manifest"
)

// DefaultSocket is the Docker Engine's unix socket.
const DefaultSocket = docker.DefaultSocket

// Limits are the resource ceilings applied to every game container.
//
// These are not tuning knobs so much as the blast radius of handing shell
// access to strangers. Every one of them is load-bearing.
type Limits struct {
	// Memory in bytes.
	Memory int64
	// NanoCPUs is CPU quota; 1e9 is one core.
	NanoCPUs int64
	// PidsLimit stops a fork bomb, which is the first thing a bored player tries.
	PidsLimit int64
	// StorageOpt caps the writable layer, e.g. {"size": "512m"}. Requires an
	// overlay2 backend on xfs with pquota; empty disables the cap.
	StorageOpt map[string]string

	// Capabilities are added back after dropping ALL. See multiUserCaps.
	Capabilities []string
}

// multiUserCaps is the capability set a multi-user box needs to be a
// multi-user box.
//
// Dropping every capability is the obvious hardening move and it is wrong here,
// for a reason that is easy to miss: an empty set stops root *relinquishing*
// privilege as well as gaining it. su(1) fails with "cannot set groups", and
// chpasswd cannot write /etc/shadow because it cannot chown its replacement to
// root:shadow. Both are load-bearing -- su is how a player moves between levels
// in a live session, and chpasswd is how a run's passwords get set at all.
//
// So the reasoning is inverted from the usual. Everything here is a power that
// uid 0 needs to administer its own users and files, and none of it is any use
// to a player who is not already root:
//
//   - SETUID, SETGID   su, login and sudo dropping to a level account
//   - CHOWN, FOWNER,
//     FSETID,
//     DAC_OVERRIDE     administering files that belong to other accounts
//   - KILL             signalling another account's processes
//   - AUDIT_WRITE      login and PAM recording a real session, so who(1) and
//     last(1) reflect reality rather than a fabrication
//   - NET_BIND_SERVICE a game service on a privileged port, such as smtp
//
// What stays dropped is what would let a player off the box or onto the
// network: SYS_ADMIN, SYS_MODULE, SYS_PTRACE, SYS_BOOT, SYS_TIME, SYS_CHROOT,
// SYS_RAWIO, NET_ADMIN, NET_RAW, MKNOD, SETFCAP, SETPCAP and the rest.
//
// A game that genuinely needs one of those -- SYS_PTRACE for a puzzle built
// around strace, say -- should ask for it explicitly through Limits, so the
// decision is visible where the game is configured.
var multiUserCaps = []string{
	"SETUID", "SETGID",
	"CHOWN", "FOWNER", "FSETID", "DAC_OVERRIDE",
	"KILL", "AUDIT_WRITE", "NET_BIND_SERVICE",
}

// DefaultLimits are deliberately tight. A player needs a shell, not a build farm.
func DefaultLimits() Limits {
	return Limits{
		Memory:       512 << 20,
		NanoCPUs:     1_000_000_000,
		PidsLimit:    256,
		Capabilities: multiUserCaps,
	}
}

// Images resolves the image that backs a level's host for a given game version.
type Images interface {
	Image(gameID string, version int, host string) (string, error)
}

// Seeder installs a run's credentials into a container freshly created from a
// shared image.
//
// An image holds no passwords -- it is shared by every run of a game version --
// so a container is not playable until it has been seeded. Seeding therefore
// happens inside container creation, and a failure there must stop the player
// being attached rather than drop them into a box full of placeholders.
type Seeder interface {
	Seed(ctx context.Context, container string, g *manifest.Game, host string, secrets *build.RunSecrets) error
}

// Docker attaches players to containers via the Engine API.
type Docker struct {
	api    *docker.Client
	log    *slog.Logger
	images Images
	seeder Seeder
	limits Limits

	// creating serialises container creation per name, so two sessions racing
	// into the same run do not both try to create its box.
	creating sync.Map // name -> *sync.Mutex
}

// Options configure a Docker runtime.
type Options struct {
	Socket string
	Images Images
	Seeder Seeder
	Limits Limits
	Logger *slog.Logger
}

// NewDocker returns a runtime talking to the local Docker engine.
func NewDocker(opts Options) (*Docker, error) {
	if opts.Images == nil {
		return nil, fmt.Errorf("runtime: an image resolver is required")
	}
	if opts.Seeder == nil {
		return nil, fmt.Errorf("runtime: a seeder is required; an unseeded container has no credentials in it")
	}
	if opts.Socket == "" {
		opts.Socket = DefaultSocket
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Limits.Memory == 0 && opts.Limits.NanoCPUs == 0 && opts.Limits.PidsLimit == 0 {
		opts.Limits = DefaultLimits()
	}
	if opts.Limits.Capabilities == nil {
		opts.Limits.Capabilities = multiUserCaps
	}

	return &Docker{
		api:    docker.New(opts.Socket),
		log:    opts.Logger,
		images: opts.Images,
		seeder: opts.Seeder,
		limits: opts.Limits,
	}, nil
}

// ContainerName is the container backing one host of one run. Deriving it from
// the run id rather than tracking it in a table means a crashed broker can
// still find, reuse and reap what it left behind.
func ContainerName(runID int64, host string) string {
	return fmt.Sprintf("wge-run-%d-%s", runID, host)
}

// Attach gives the player a shell on their level, creating the container first
// if this is their first connection since it was last reaped.
func (d *Docker) Attach(ctx context.Context, s *broker.Session) (int, error) {
	image, err := d.images.Image(s.Game.ID, s.Run.GameVersion, s.Level.Host)
	if err != nil {
		return 0, fmt.Errorf("resolve image: %w", err)
	}

	name := ContainerName(s.Run.ID, s.Level.Host)
	if err := d.ensureContainer(ctx, name, image, s); err != nil {
		return 0, err
	}

	execID, err := d.createExec(ctx, name, s)
	if err != nil {
		return 0, fmt.Errorf("create exec: %w", err)
	}

	if err := d.runExec(ctx, execID, s); err != nil {
		return 0, err
	}
	return d.execExitCode(ctx, execID)
}

// ensureContainer creates and starts the run's container if it is not already
// running. Creation is idempotent by name.
func (d *Docker) ensureContainer(ctx context.Context, name, image string, s *broker.Session) error {
	mu, _ := d.creating.LoadOrStore(name, &sync.Mutex{})
	lock := mu.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	state, err := d.inspectState(ctx, name)
	switch {
	case err == nil && state.Running:
		return nil
	case err == nil:
		// Present but stopped: a rebuild is cheaper and more predictable than
		// restarting something in an unknown state.
		if err := d.remove(ctx, name); err != nil {
			return fmt.Errorf("remove stale container: %w", err)
		}
	case !docker.IsNotFound(err):
		return fmt.Errorf("inspect container: %w", err)
	}

	if err := d.create(ctx, name, image, s); err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if err := d.api.Post(ctx, "/containers/"+name+"/start", nil, nil); err != nil {
		return fmt.Errorf("start container: %w", err)
	}

	// Seed before anyone can reach the box. The player has no way to run
	// anything until the attach below, so the window between start and seed is
	// not observable from inside -- but it must still be closed before the
	// first exec, or they arrive to find templates where the game should be.
	seedCtx, cancel := context.WithTimeout(ctx, build.SeedTimeout)
	defer cancel()

	secrets := build.NewRunSecrets(s.Game, s.Run.Deriver(), s.Player.Handle)
	if err := d.seeder.Seed(seedCtx, name, s.Game, s.Level.Host, secrets); err != nil {
		// A half-seeded container is worse than none: remove it so the next
		// attempt builds a clean one rather than reusing this.
		if rmErr := d.remove(ctx, name); rmErr != nil && !docker.IsNotFound(rmErr) {
			d.log.Error("remove unseeded container", "name", name, "error", rmErr)
		}
		return fmt.Errorf("seed container: %w", err)
	}

	d.log.Info("container created", "name", name, "image", image, "run", s.Run.ID)
	return nil
}

type containerState struct {
	Running bool
}

type inspectResponse struct {
	State containerState
}

func (d *Docker) inspectState(ctx context.Context, name string) (containerState, error) {
	var resp inspectResponse
	err := d.api.Get(ctx, "/containers/"+name+"/json", &resp)
	return resp.State, err
}

func (d *Docker) create(ctx context.Context, name, image string, s *broker.Session) error {
	body := map[string]any{
		"Image":        image,
		"Hostname":     s.Level.Host,
		"Tty":          false,
		"OpenStdin":    false,
		"AttachStdout": false,
		"AttachStderr": false,
		// The container runs its own init and services; players are attached
		// with exec afterwards.
		"Labels": map[string]string{
			"wge.run":  fmt.Sprint(s.Run.ID),
			"wge.game": s.Game.ID,
			"wge.host": s.Level.Host,
		},
		"HostConfig": d.hostConfig(),
	}

	q := url.Values{"name": {name}}
	return d.api.Post(ctx, "/containers/create?"+q.Encode(), body, nil)
}

// hostConfig is the sandbox. Every field here exists because the container is
// hostile by design: the player is invited to attack it.
func (d *Docker) hostConfig() map[string]any {
	caps := d.limits.Capabilities
	if caps == nil {
		caps = multiUserCaps
	}

	cfg := map[string]any{
		// Drop everything, then add back only what lets root become somebody
		// else. See multiUserCaps for why an empty set is the wrong answer.
		"CapDrop": []string{"ALL"},
		"CapAdd":  caps,
		// A setuid binary inside must not be able to raise privilege beyond
		// what the game intends.
		"SecurityOpt": []string{"no-new-privileges"},
		// No egress. An unfiltered shell box on the internet becomes someone
		// else's spam relay within the week. Multi-host games attach a private
		// per-run network instead of the default bridge.
		"NetworkMode": "none",
		"Memory":      d.limits.Memory,
		"NanoCpus":    d.limits.NanoCPUs,
		"PidsLimit":   d.limits.PidsLimit,
		// A game box that survives a reboot is a game box nobody rebuilt from
		// the salt; restarts must go through creation.
		"RestartPolicy": map[string]any{"Name": "no"},
	}
	if len(d.limits.StorageOpt) > 0 {
		cfg["StorageOpt"] = d.limits.StorageOpt
	}
	return cfg
}

type execConfig struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	User         string   `json:"User"`
	Env          []string `json:"Env"`
	Cmd          []string `json:"Cmd"`
}

type execCreateResponse struct {
	ID string `json:"Id"`
}

func (d *Docker) createExec(ctx context.Context, name string, s *broker.Session) (string, error) {
	cmd := []string{"login", "-f", s.Level.User}
	if s.Command != "" {
		// Non-interactive `ssh host <cmd>`. Run it as the level user through a
		// login shell so the environment matches an interactive session.
		cmd = []string{"su", "-l", s.Level.User, "-c", s.Command}
	}

	cfg := execConfig{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          s.Command == "",
		// Enter as root so login(1) can drop to the level user with a full,
		// genuine session: real utmp, real PAM, real environment.
		User: "root",
		Env:  []string{"TERM=" + s.Term},
		Cmd:  cmd,
	}

	var resp execCreateResponse
	if err := d.api.Post(ctx, "/containers/"+name+"/exec", cfg, &resp); err != nil {
		return "", err
	}
	return resp.ID, nil
}

// runExec starts the exec and pumps bytes between the SSH channel and the
// container until either side closes.
func (d *Docker) runExec(ctx context.Context, execID string, s *broker.Session) error {
	conn, br, err := d.api.Hijack(ctx, "/exec/"+execID+"/start", map[string]any{
		"Detach": false,
		"Tty":    s.Command == "",
	})
	if err != nil {
		return fmt.Errorf("start exec: %w", err)
	}
	defer conn.Close()

	// Match the container's idea of the terminal to the player's before any
	// output is produced, so full-screen programs draw correctly from the start.
	if s.Command == "" {
		d.resize(ctx, execID, s.Width, s.Height)
	}

	resizeDone := make(chan struct{})
	defer close(resizeDone)
	go func() {
		for {
			select {
			case size, ok := <-s.Resize:
				if !ok {
					return
				}
				d.resize(ctx, execID, size.Width, size.Height)
			case <-resizeDone:
				return
			}
		}
	}()

	go func() {
		_, err := io.Copy(conn, s.Stdin)
		// Signal EOF to the process without tearing down the read side: a
		// non-interactive command sends EOF immediately and its output has not
		// been written yet.
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		if err != nil {
			// The player hung up. Drop the connection so the output copy below
			// unblocks instead of waiting on a shell nobody is reading.
			conn.Close()
		}
	}()

	// The output side is what actually ends a session: it returns when the
	// process exits and the engine closes the stream. Waiting on whichever
	// direction finished first would cut a command off before it had answered.
	output := make(chan error, 1)
	go func() {
		if s.Command == "" {
			// A TTY stream is raw in both directions.
			_, err := io.Copy(s.Stdout, br)
			output <- err
			return
		}
		output <- demultiplex(br, s.Stdout, s.Stderr)
	}()

	select {
	case err := <-output:
		if err != nil && !errors.Is(err, io.EOF) {
			d.log.Debug("session stream ended", "exec", execID, "error", err)
		}
	case <-ctx.Done():
	}
	return nil
}

// dockerStreamHeader is the framing Docker applies when an exec has no TTY:
// one byte of stream id, three reserved, then a big-endian payload length.
const dockerStreamHeader = 8

// demultiplex splits a non-TTY exec stream back into stdout and stderr.
// Without it the frame headers would be written to the player's terminal as
// stray control bytes ahead of every chunk of output.
func demultiplex(r io.Reader, stdout, stderr io.Writer) error {
	header := make([]byte, dockerStreamHeader)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}

		w := stdout
		if header[0] == 2 {
			w = stderr
		}

		size := int64(binary.BigEndian.Uint32(header[4:]))
		if _, err := io.CopyN(w, r, size); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (d *Docker) resize(ctx context.Context, execID string, w, h int) {
	if w <= 0 || h <= 0 {
		return
	}
	q := url.Values{"w": {fmt.Sprint(w)}, "h": {fmt.Sprint(h)}}
	if err := d.api.Post(ctx, "/exec/"+execID+"/resize?"+q.Encode(), nil, nil); err != nil {
		// A failed resize is cosmetic; it must never end a session.
		d.log.Debug("resize exec", "exec", execID, "error", err)
	}
}

type execInspectResponse struct {
	ExitCode int  `json:"ExitCode"`
	Running  bool `json:"Running"`
}

func (d *Docker) execExitCode(ctx context.Context, execID string) (int, error) {
	var resp execInspectResponse
	if err := d.api.Get(ctx, "/exec/"+execID+"/json", &resp); err != nil {
		return 0, err
	}
	if resp.Running {
		return 0, nil
	}
	return resp.ExitCode, nil
}

// Reap destroys a run's container. The scratch volume, if any, outlives it.
func (d *Docker) Reap(ctx context.Context, runID int64, host string) error {
	name := ContainerName(runID, host)
	if err := d.remove(ctx, name); err != nil && !docker.IsNotFound(err) {
		return err
	}
	d.creating.Delete(name)
	d.log.Info("container reaped", "name", name)
	return nil
}

func (d *Docker) remove(ctx context.Context, name string) error {
	q := url.Values{"force": {"true"}, "v": {"false"}}
	return d.api.Delete(ctx, "/containers/"+name+"?"+q.Encode())
}
