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
	"os"
	"strconv"
	"sync"
	"time"

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
	"KILL", "AUDIT_WRITE", "NET_BIND_SERVICE", "SYS_CHROOT",
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

// Runs records which machine is holding a run's containers, so a returning
// player is sent back to the box they left rather than to a rebuilt one.
type Runs interface {
	SetHost(ctx context.Context, runID int64, node string) error

	// RecordProgress notes that a run reached a level. It is called for levels
	// the player got to from inside the run, which the front door never sees.
	RecordProgress(ctx context.Context, runID int64, levelID string) error
}

// Docker attaches players to containers via the Engine API.
type Docker struct {
	api    *docker.Client
	log    *slog.Logger
	images Images
	seeder Seeder
	runs   Runs
	limits Limits

	// node identifies this machine in the runs table.
	node   string
	reaper *reaper
	sweep  time.Duration

	// authOffsets records how far each machine's auth log had got when it
	// finished booting, so that live sessions can be told from the history the
	// aging pass wrote.
	authOffsets sync.Map

	// creating serialises container creation per name, so two sessions racing
	// into the same run do not both try to create its box.
	creating sync.Map // name -> *sync.Mutex
}

// Options configure a Docker runtime.
type Options struct {
	Socket string
	Images Images
	Seeder Seeder
	// Runs is optional; without it sticky routing is not recorded.
	Runs Runs
	// Node names this machine. Defaults to the system hostname.
	Node   string
	Limits Limits
	// Grace is how long a container outlives its last session.
	Grace time.Duration
	// Sweep is how often orphaned containers are collected.
	Sweep  time.Duration
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

	if opts.Node == "" {
		host, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("runtime: determine node name: %w", err)
		}
		opts.Node = host
	}
	if opts.Sweep <= 0 {
		opts.Sweep = DefaultSweep
	}

	d := &Docker{
		api:    docker.New(opts.Socket),
		log:    opts.Logger,
		images: opts.Images,
		seeder: opts.Seeder,
		runs:   opts.Runs,
		limits: opts.Limits,
		node:   opts.Node,
		sweep:  opts.Sweep,
	}
	d.reaper = newReaper(opts.Grace, d.destroy)
	return d, nil
}

// networkMode is the network a container is created on.
func networkMode(networks []string) string {
	if len(networks) == 0 {
		return "none"
	}
	return networks[0]
}

// ContainerName is the container backing one host of one run. Deriving it from
// the run id rather than tracking it in a table means a crashed broker can
// still find, reuse and reap what it left behind.
func ContainerName(runID int64, host string) string {
	return fmt.Sprintf("wge-run-%d-%s", runID, host)
}

// Attach gives the player a shell on their level, building the run's machines
// first if this is their first connection since they were last reaped.
func (d *Docker) Attach(ctx context.Context, s *broker.Session) (int, error) {
	plan := planTopology(s.Game, s.Run.ID)

	// Every container in the run is claimed, not just the one the player lands
	// on. Pivoting needs the peers to be up, and a peer nothing is holding
	// would be taken by the next sweep while the player was still on the first
	// box looking for the way across.
	var held []target
	for _, host := range plan.hosts {
		t := target{Name: ContainerName(s.Run.ID, host), RunID: s.Run.ID}
		d.reaper.hold(t)
		held = append(held, t)
	}
	defer func() {
		// Before letting go: read back which levels the player reached from
		// inside the run, which is the only place that can be seen.
		d.collectProgress(s)

		for _, t := range held {
			d.reaper.release(t)
		}
	}()

	if err := d.ensureTopology(ctx, s, plan); err != nil {
		return 0, err
	}

	name := ContainerName(s.Run.ID, s.Level.Host)

	execID, err := d.createExec(ctx, name, s)
	if err != nil {
		return 0, fmt.Errorf("create exec: %w", err)
	}

	if err := d.runExec(ctx, execID, s); err != nil {
		return 0, err
	}
	return d.execExitCode(ctx, execID)
}

// ensureTopology brings up the run's networks and every machine on them.
func (d *Docker) ensureTopology(ctx context.Context, s *broker.Session, plan topology) error {
	labels := map[string]string{"wge.run": fmt.Sprint(s.Run.ID), "wge.game": s.Game.ID}

	for _, network := range plan.networks {
		if err := d.api.CreateNetwork(ctx, network, labels); err != nil {
			return fmt.Errorf("create network %s: %w", network, err)
		}
	}

	for _, host := range plan.hosts {
		if err := d.ensureContainer(ctx, s, host, plan.byHost[host]); err != nil {
			return fmt.Errorf("host %s: %w", host, err)
		}
	}
	return nil
}

// ensureContainer creates and starts one of a run's machines if it is not
// already up. Creation is idempotent by name.
func (d *Docker) ensureContainer(ctx context.Context, s *broker.Session, host string, networks []string) error {
	name := ContainerName(s.Run.ID, host)

	image, err := d.images.Image(s.Game.ID, s.Run.GameVersion, host)
	if err != nil {
		return fmt.Errorf("resolve image: %w", err)
	}

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

	if err := d.create(ctx, name, image, host, networks, s); err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if err := d.api.Post(ctx, "/containers/"+name+"/start", nil, nil); err != nil {
		return fmt.Errorf("start container: %w", err)
	}

	// The first network is attached at creation; the rest have to be joined
	// afterwards. The alias is the host's name, which is what makes `ssh vault`
	// resolve from another machine in the run and nowhere else.
	for _, network := range networks[min(1, len(networks)):] {
		if err := d.api.ConnectNetwork(ctx, network, name, []string{host}); err != nil {
			return fmt.Errorf("join network %s: %w", network, err)
		}
	}

	// Seed before anyone can reach the box. The player has no way to run
	// anything until the attach, so the window between start and seed is not
	// observable from inside -- but it must still be closed before the first
	// exec, or they arrive to find templates where the game should be.
	seedCtx, cancel := context.WithTimeout(ctx, build.SeedTimeout)
	defer cancel()

	secrets := build.NewRunSecrets(s.Game, s.Run.Deriver(), s.Player.Handle)
	if err := d.seeder.Seed(seedCtx, name, s.Game, host, secrets); err != nil {
		// A half-seeded container is worse than none: remove it so the next
		// attempt builds a clean one rather than reusing this.
		if rmErr := d.remove(ctx, name); rmErr != nil && !docker.IsNotFound(rmErr) {
			d.log.Error("remove unseeded container", "name", name, "error", rmErr)
		}
		return fmt.Errorf("seed container: %w", err)
	}

	// Sticky routing: while this container is alive the player comes back here.
	if d.runs != nil {
		if err := d.runs.SetHost(ctx, s.Run.ID, d.node); err != nil {
			d.log.Error("record run placement", "run", s.Run.ID, "error", err)
		}
	}

	// Wait for init to finish before anyone can look at the machine. A player
	// who lands on a box mid-boot finds its services reported as not running,
	// and one who already knows the way across finds the peer refusing
	// connections -- both of which are the engine's startup showing through.
	d.waitForBoot(ctx, name)

	// Everything the auth log gains from here happened during play.
	d.markLive(ctx, name)

	d.log.Info("machine created", "name", name, "image", image, "host", host, "run", s.Run.ID)
	return nil
}

// BootTimeout bounds how long a machine is given to finish starting.
const BootTimeout = 25 * time.Second

// waitForBoot blocks until the container's init has run the boot sequence out.
//
// sysvinit's rc script is the whole of it: while that process is alive the
// runlevel is still being entered, and when it exits every service that is
// going to start has started.
func (d *Docker) waitForBoot(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(ctx, BootTimeout)
	defer cancel()

	const check = `pgrep -f "/etc/init.d/rc " >/dev/null 2>&1 && exit 1; exit 0`

	for {
		res, err := d.api.Exec(ctx, name, docker.ExecOptions{
			Cmd: []string{"sh", "-c", check}, User: "root",
		})
		if err == nil && res.ExitCode == 0 {
			return
		}
		if err != nil {
			// The container may not be accepting execs yet; that is itself a
			// reason to keep waiting.
			d.log.Debug("waiting for boot", "name", name, "error", err)
		}

		select {
		case <-ctx.Done():
			d.log.Warn("machine did not finish booting in time", "name", name)
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// destroy removes a container whose grace period has run out, and the run's
// networks once its last machine is gone.
func (d *Docker) destroy(t target) {
	// Deliberately not the session's context: the session is what ended.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := d.remove(ctx, t.Name); err != nil && !docker.IsNotFound(err) {
		d.log.Error("reap container", "name", t.Name, "error", err)
		return
	}
	d.reaper.forget(t.Name)
	d.creating.Delete(t.Name)
	d.authOffsets.Delete(t.Name)

	// A run spanning several machines keeps its placement, and its networks,
	// until the last of them is gone.
	remaining, err := d.api.ListContainers(ctx, fmt.Sprintf("wge.run=%d", t.RunID))
	if err != nil {
		d.log.Error("count remaining containers", "run", t.RunID, "error", err)
		return
	}
	if len(remaining) > 0 {
		return
	}

	d.removeNetworks(ctx, t.RunID)

	if d.runs != nil {
		if err := d.runs.SetHost(ctx, t.RunID, ""); err != nil {
			d.log.Error("clear run placement", "run", t.RunID, "error", err)
		}
	}

	d.log.Info("run reaped", "run", t.RunID)
}

// Run collects abandoned machines until the context is cancelled.
//
// The first sweep happens immediately, which means a starting engine clears
// whatever a previous process left behind. That is the right thing rather than
// a compromise: nothing knows whether those containers still have players
// behind them, and rebuilding one costs a reconnection.
func (d *Docker) Run(ctx context.Context) {
	ticker := time.NewTicker(d.sweep)
	defer ticker.Stop()

	d.sweepOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			d.reaper.stop()
			return
		case <-ticker.C:
			d.sweepOnce(ctx)
		}
	}
}

// sweepOnce destroys every game container no session is holding, and then any
// network left with nothing on it.
func (d *Docker) sweepOnce(ctx context.Context) {
	containers, err := d.api.ListContainers(ctx, "wge.run")
	if err != nil {
		d.log.Error("sweep containers", "error", err)
		return
	}

	held := d.reaper.heldNames()
	liveRuns := map[int64]bool{}

	for _, c := range containers {
		name := c.Name()
		if name == "" {
			continue
		}

		runID, err := strconv.ParseInt(c.Labels["wge.run"], 10, 64)
		if err != nil {
			d.log.Error("container has an unreadable run label", "name", name, "label", c.Labels["wge.run"])
			continue
		}
		if held[name] {
			liveRuns[runID] = true
			continue
		}

		d.log.Info("collecting abandoned machine", "name", name, "run", runID)
		d.destroy(target{Name: name, RunID: runID})
	}

	d.sweepNetworks(ctx, liveRuns)
}

// sweepNetworks removes the networks of runs that have no machines left.
func (d *Docker) sweepNetworks(ctx context.Context, liveRuns map[int64]bool) {
	networks, err := d.api.ListNetworks(ctx, "wge.run")
	if err != nil {
		d.log.Error("sweep networks", "error", err)
		return
	}

	for _, network := range networks {
		runID, err := strconv.ParseInt(network.Labels["wge.run"], 10, 64)
		if err != nil || liveRuns[runID] {
			continue
		}
		if err := d.api.RemoveNetwork(ctx, network.Name); err != nil && !docker.IsNotFound(err) {
			d.log.Debug("remove abandoned network", "name", network.Name, "error", err)
		}
	}
}

// removeNetworks deletes a run's private networks. Nothing may be attached to
// a network for it to go, which is why the containers are removed first.
func (d *Docker) removeNetworks(ctx context.Context, runID int64) {
	networks, err := d.api.ListNetworks(ctx, fmt.Sprintf("wge.run=%d", runID))
	if err != nil {
		d.log.Error("list run networks", "run", runID, "error", err)
		return
	}
	for _, network := range networks {
		if err := d.api.RemoveNetwork(ctx, network.Name); err != nil && !docker.IsNotFound(err) {
			d.log.Error("remove network", "name", network.Name, "error", err)
		}
	}
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

func (d *Docker) create(ctx context.Context, name, image, host string, networks []string, s *broker.Session) error {
	body := map[string]any{
		"Image":        image,
		"Hostname":     host,
		"Tty":          false,
		"OpenStdin":    false,
		"AttachStdout": false,
		"AttachStderr": false,
		// The container runs its own init and services; players are attached
		// with exec afterwards.
		"Labels": map[string]string{
			"wge.run":  fmt.Sprint(s.Run.ID),
			"wge.game": s.Game.ID,
			"wge.host": host,
		},
		"HostConfig": d.hostConfig(networks),
	}

	if len(networks) > 0 {
		body["NetworkingConfig"] = map[string]any{
			"EndpointsConfig": map[string]any{
				networks[0]: map[string]any{"Aliases": []string{host}},
			},
		}
	}

	q := url.Values{"name": {name}}
	return d.api.Post(ctx, "/containers/create?"+q.Encode(), body, nil)
}

// hostConfig is the sandbox. Every field here exists because the container is
// hostile by design: the player is invited to attack it.
func (d *Docker) hostConfig(networks []string) map[string]any {
	caps := d.limits.Capabilities
	if caps == nil {
		caps = multiUserCaps
	}

	cfg := map[string]any{
		// Drop everything, then add back only what lets root become somebody
		// else. See multiUserCaps for why an empty set is the wrong answer.
		"CapDrop": []string{"ALL"},
		"CapAdd":  caps,
		// no-new-privileges is deliberately NOT set.
		//
		// It is the reflex hardening flag for a container, and on a multi-user
		// box it breaks the game. The flag makes the kernel ignore the setuid
		// bit, so su(1) cannot become root to read /etc/shadow and fails with
		// "Authentication failure", and sudo refuses to run at all. Moving
		// between levels in a live session is exactly su, so the flag turns off
		// the central mechanic.
		//
		// What it would have protected against is a player finding a setuid
		// binary and becoming root inside the container. That is the game
		// working as intended, and it is not a way out of the container -- the
		// capability set is what stops that. The compensating control is the
		// setuid audit in `wge test`, which proves the box carries exactly the
		// setuid binaries the base image ships and nothing an author added by
		// accident.
		// No route off the run. A lone machine gets no network at all; a run
		// with several gets private internal networks, which have no path to
		// the host or the internet. An unfiltered shell box becomes somebody
		// else's spam relay within the week.
		"NetworkMode": networkMode(networks),
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

func (d *Docker) remove(ctx context.Context, name string) error {
	q := url.Values{"force": {"true"}, "v": {"false"}}
	return d.api.Delete(ctx, "/containers/"+name+"?"+q.Encode())
}
