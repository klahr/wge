package runtime

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/klahr/wge/internal/broker"
	"github.com/klahr/wge/internal/docker"
)

// authLog is where the box records who opened a session.
//
// Both routes between levels end up here: sshd writing
// "pam_unix(sshd:session): session opened for user dsundqvist", and su(1)
// writing the same line with its own service name. The binary wtmp records the
// first and not the second, so this is the only file that sees a player move
// either way.
const authLog = "/var/log/auth.log"

// sessionOpened matches PAM's session line, whatever opened it.
var sessionOpened = regexp.MustCompile(
	`pam_unix\([a-z0-9_-]+:session\): session opened for user ([a-z_][a-z0-9_-]*)`)

// parseSessions returns the accounts named in a run of auth.log lines, in the
// order they first appear.
func parseSessions(log string) []string {
	var users []string
	seen := map[string]bool{}

	for _, match := range sessionOpened.FindAllStringSubmatch(log, -1) {
		user := match[1]
		if !seen[user] {
			seen[user] = true
			users = append(users, user)
		}
	}
	return users
}

// markLive records where a machine's auth.log had got to by the time it
// finished booting.
//
// Everything past that point happened during play, which is what makes the
// history the aging pass wrote indistinguishable from the live entries for
// every purpose except this one. No timestamp is parsed: syslog lines carry no
// year, and an offset taken at a known moment is exact where a parsed date
// would be a guess.
func (d *node) markLive(ctx context.Context, container string) {
	size, err := d.fileSize(ctx, container, authLog)
	if err != nil {
		d.log.Debug("read auth log size", "name", container, "error", err)
		return
	}
	d.authOffsets.Store(container, size)
}

func (d *node) fileSize(ctx context.Context, container, path string) (int64, error) {
	res, err := d.api.Exec(ctx, container, docker.ExecOptions{
		Cmd:  []string{"stat", "-c", "%s", path},
		User: "root",
	})
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64)
}

// collectProgress records the levels a player reached without coming back
// through the front door.
//
// The broker sees a level when it attaches somebody to one, which misses every
// transition made from inside: su between two accounts on one machine, and ssh
// to another machine in the run. For a level on a host the front door will not
// attach to, that is the difference between recorded and never recorded at all.
//
// The box already knows. Nothing is installed to find out, and there is no
// agent for a player to notice.
func (d *node) collectProgress(s *broker.Session) {
	if d.runs == nil {
		return
	}

	// The session's context is finished; this outlives it deliberately.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	levels := map[string]map[string]string{} // host -> user -> level id
	for _, l := range s.Game.Levels {
		if levels[l.Host] == nil {
			levels[l.Host] = map[string]string{}
		}
		levels[l.Host][l.User] = l.ID
	}

	for _, host := range planTopology(s.Game, s.Run.ID).hosts {
		container := ContainerName(s.Run.ID, host)

		from, ok := d.authOffsets.Load(container)
		if !ok {
			continue // never marked, so nothing can be said to be live
		}
		offset := from.(int64)

		res, err := d.api.Exec(ctx, container, docker.ExecOptions{
			// +1 because tail counts from one, not from zero.
			Cmd:  []string{"tail", "-c", "+" + strconv.FormatInt(offset+1, 10), authLog},
			User: "root",
		})
		if err != nil {
			d.log.Debug("read auth log", "name", container, "error", err)
			continue
		}

		for _, user := range parseSessions(res.Stdout) {
			level, ok := levels[host][user]
			if !ok {
				continue // a service account, a decoy, or root
			}
			if err := d.runs.RecordProgress(ctx, s.Run.ID, level); err != nil {
				d.log.Error("record progress", "run", s.Run.ID, "level", level, "error", err)
				continue
			}
			d.log.Info("level reached", "run", s.Run.ID, "level", level, "host", host)
		}

		// Advance the mark so the next session starts where this one stopped.
		if size, err := d.fileSize(ctx, container, authLog); err == nil {
			d.authOffsets.Store(container, size)
		}
	}
}
