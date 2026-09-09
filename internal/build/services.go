package build

import (
	"bytes"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/klahr/wge/internal/manifest"
)

// runlevels a service is started in, and the ones it is stopped in. Debian's
// default runlevel is 2; the rest are declared because that is what a real
// init script declares, and a player who reads the LSB header should find what
// they expect.
var (
	startRunlevels = []string{"2", "3", "4", "5"}
	stopRunlevels  = []string{"0", "1", "6"}
)

// registerServices runs update-rc.d for each service.
//
// Creating the rc*.d symlinks directly does not work, and fails silently.
// Debian's /etc/init.d/rc runs with CONCURRENCY=makefile, which means startpar
// drives the boot from /etc/init.d/.depend.start -- a file insserv generates
// from the LSB headers. A symlink that is not also in that file is simply never
// run, with nothing logged to say so. update-rc.d writes both.
func (p *Plan) registerServices() string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -eu\n\n")

	for _, l := range p.Game.Levels {
		if l.Host != p.Host {
			continue
		}
		for _, svc := range l.Services {
			fmt.Fprintf(&b, "update-rc.d %s defaults\n", svc.Name)
		}
	}
	return b.String()
}

// addServices installs each declared service as an init script, wires it into
// the runlevels, and gives it a log that already has a history.
//
// Services are what give a box a pulse. A machine where nothing is listening,
// nothing is running and no log has grown since the image was built is a
// machine nobody uses -- and `ss -tlnp`, `ps aux` and a tail of /var/log are
// among the first things a player reaches for.
func (p *Plan) addServices() error {
	byUser := map[string]*account{}
	for _, a := range p.Accounts {
		byUser[a.User] = a
	}

	for _, l := range p.Game.Levels {
		if l.Host != p.Host {
			continue
		}

		owner, ok := byUser[l.User]
		if !ok {
			continue
		}

		for _, svc := range l.Services {
			runAs, ok := byUser[svc.User]
			if !ok {
				return fmt.Errorf("level %s: service %q runs as unknown account %q", l.ID, svc.Name, svc.User)
			}

			script := path.Join("/etc/init.d", svc.Name)
			at := p.Aging.FileTime(Owner{User: "root"}, script)

			p.rootfs.add(entry{
				Path: script, Content: []byte(initScript(svc)),
				Mode: 0o755, UID: 0, GID: 0, ModTime: at,
			})

			// The daemon appends to its log as its own user, and the level's
			// scheduled jobs append to it as theirs, so the group needs write
			// as well as read. Without it a cron job's output vanishes: the
			// redirect fails and the shell reports it to a stderr nobody sees.
			p.rootfs.add(entry{
				Path: svc.LogPath(), Content: p.serviceLog(svc),
				Mode: 0o660, UID: runAs.UID, GID: owner.GID,
				ModTime: p.Aging.LogTime("service/"+svc.Name, 9, 10),
			})
		}
	}
	return nil
}

// initScript renders an LSB init script.
//
// It is a real one, not a shim: service(8), /etc/init.d/<name> status and the
// boot sequence all go through it, and start-stop-daemon does the work exactly
// as it would on any Debian machine.
func initScript(svc manifest.Service) string {
	description := svc.Description
	if description == "" {
		description = svc.Name
	}

	command := svc.Exec
	if len(svc.Args) > 0 {
		command += " " + strings.Join(svc.Args, " ")
	}

	var b strings.Builder
	fmt.Fprintf(&b, `#!/bin/sh
### BEGIN INIT INFO
# Provides:          %s
# Required-Start:    $local_fs $remote_fs $syslog
# Required-Stop:     $local_fs $remote_fs $syslog
# Default-Start:     %s
# Default-Stop:      %s
# Short-Description: %s
### END INIT INFO

NAME=%s
DESC="%s"
DAEMON_USER=%s
PIDFILE=/var/run/$NAME.pid
LOGFILE=%s

. /lib/lsb/init-functions

case "$1" in
  start)
	log_daemon_msg "Starting $DESC" "$NAME"
	start-stop-daemon --start --quiet --background --make-pidfile \
		--pidfile "$PIDFILE" --chuid "$DAEMON_USER" \
		--startas /bin/sh -- -c 'exec %s >> '"$LOGFILE"' 2>&1'
	log_end_msg $?
	;;
  stop)
	log_daemon_msg "Stopping $DESC" "$NAME"
	start-stop-daemon --stop --quiet --oknodo --retry 5 --pidfile "$PIDFILE"
	rm -f "$PIDFILE"
	log_end_msg $?
	;;
  restart|force-reload)
	"$0" stop
	sleep 1
	"$0" start
	;;
  status)
	status_of_proc -p "$PIDFILE" "$NAME" "$NAME" && exit 0 || exit $?
	;;
  *)
	echo "Usage: $0 {start|stop|restart|force-reload|status}" >&2
	exit 2
	;;
esac

exit 0
`,
		svc.Name,
		strings.Join(startRunlevels, " "),
		strings.Join(stopRunlevels, " "),
		description,
		svc.Name,
		description,
		svc.User,
		svc.LogPath(),
		command,
	)
	return b.String()
}

// serviceLogLines are what a daemon of this sort is imagined to have said.
var serviceLogLines = []string{
	"started, pid %d",
	"configuration reloaded",
	"run complete in %ds",
	"nothing to do",
	"deferred until next window",
	"warning: staging area above 80%% capacity",
	"run complete in %ds",
	"started, pid %d",
}

// serviceLog gives a service's log a history, so that a box being served for
// the first time does not present a daemon that has apparently never run.
func (p *Plan) serviceLog(svc manifest.Service) []byte {
	const lines = 40

	var b bytes.Buffer
	times := p.Aging.Spread("service/"+svc.Name, lines)

	for i, at := range times {
		template := serviceLogLines[i%len(serviceLogLines)]
		value := 1000 + int(p.Aging.fraction("svclog", fmt.Sprintf("%s/%d", svc.Name, i))*8000)

		message := template
		if strings.Contains(template, "%d") {
			message = fmt.Sprintf(template, value)
		} else {
			message = strings.ReplaceAll(template, "%%", "%")
		}

		fmt.Fprintf(&b, "%s %s[%d]: %s\n",
			at.Format("2006-01-02 15:04:05"), svc.Name, 900+value%400, message)
	}
	return b.Bytes()
}

// addCron installs the levels' scheduled jobs.
//
// Entries go in /etc/cron.d rather than a user crontab: the files are visible,
// readable and obviously part of the system, which is what makes a scheduled
// job a puzzle surface rather than a hidden mechanism.
func (p *Plan) addCron() {
	for _, l := range p.Game.Levels {
		if l.Host != p.Host || len(l.Cron) == 0 {
			continue
		}

		var b bytes.Buffer
		b.WriteString("# Scheduled work for " + l.User + ".\n")
		b.WriteString("SHELL=/bin/sh\n")
		b.WriteString("PATH=/usr/local/sbin:/usr/local/bin:/sbin:/bin:/usr/sbin:/usr/bin\n")
		b.WriteString("MAILTO=\"\"\n\n")

		for _, job := range l.Cron {
			user := job.User
			if user == "" {
				user = l.User
			}
			if job.Comment != "" {
				fmt.Fprintf(&b, "# %s\n", job.Comment)
			}
			fmt.Fprintf(&b, "%s %s %s\n", job.Schedule, user, job.Command)
		}

		file := path.Join("/etc/cron.d", l.ID)
		p.rootfs.add(entry{
			Path: file, Content: b.Bytes(),
			// cron refuses to run a file in cron.d that anybody but root can
			// write, and ignores it silently, so the mode is load-bearing.
			Mode: 0o644, UID: 0, GID: 0,
			ModTime: p.Aging.FileTime(Owner{User: "root"}, file),
		})
	}
}

// ServiceAccounts returns the service accounts on this host, for the verifier.
func (p *Plan) ServiceAccounts() []string {
	var out []string
	for _, a := range p.Accounts {
		if a.Service {
			out = append(out, a.User)
		}
	}
	sort.Strings(out)
	return out
}

var _ = time.Time{}

// dailyAt parses the common cron schedule shapes into a time of day.
//
// The historical syslog has to show the scheduled jobs having actually run,
// and at the times the crontab says they run. An approximation would be a new
// inconsistency: a player who reads /etc/cron.d and then greps the log for the
// job would find it firing at the wrong minute.
func dailyAt(schedule string) (hour, minute int, ok bool) {
	switch strings.TrimSpace(schedule) {
	case "@daily", "@midnight":
		return 0, 0, true
	case "@hourly", "@reboot", "@weekly", "@monthly", "@yearly", "@annually":
		// Not daily; the caller falls back to scattering entries.
		return 0, 0, false
	}

	fields := strings.Fields(schedule)
	if len(fields) != 5 {
		return 0, 0, false
	}
	// Only a fixed minute and hour running every day is treated as daily.
	if fields[2] != "*" || fields[3] != "*" || fields[4] != "*" {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(fields[0], "%d", &minute); err != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(fields[1], "%d", &hour); err != nil {
		return 0, 0, false
	}
	if minute < 0 || minute > 59 || hour < 0 || hour > 23 {
		return 0, 0, false
	}
	return hour, minute, true
}

// syslogHistory writes the log the syslog daemon will go on appending to.
//
// Without it, a box with months of auth.log presents a /var/log/syslog that
// begins the moment the container started -- which is to say, the moment this
// player's game was created. The entries are the scheduled jobs actually
// firing, at the times /etc/cron.d says they fire, so the two agree.
func (p *Plan) syslogHistory() []byte {
	type line struct {
		at   time.Time
		text string
	}
	var lines []line

	// A fortnight of nightly runs is enough to look routine without being a
	// wall of text.
	const days = 14
	present := p.Aging.Now()

	for _, l := range p.Game.Levels {
		if l.Host != p.Host {
			continue
		}
		for _, job := range l.Cron {
			user := job.User
			if user == "" {
				user = l.User
			}
			hour, minute, daily := dailyAt(job.Schedule)
			if !daily {
				continue
			}

			for d := days; d >= 1; d-- {
				day := present.AddDate(0, 0, -d)
				at := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, day.Location())
				if at.After(present) {
					continue
				}

				pid := 3000 + int(p.Aging.fraction("cronpid", fmt.Sprintf("%s/%d", job.Command, d))*4000)
				lines = append(lines, line{at, fmt.Sprintf(
					"CRON[%d]: (%s) CMD (%s)", pid, user, job.Command)})
			}
		}
	}

	// Something has to have started the logger, or the log begins mid-thought.
	lines = append(lines, line{p.Aging.Start(), "syslogd: restart"})

	sort.Slice(lines, func(i, j int) bool { return lines[i].at.Before(lines[j].at) })

	var b bytes.Buffer
	for _, l := range lines {
		fmt.Fprintf(&b, "%s %s %s\n", l.at.Format("Jan _2 15:04:05"), p.Host, l.text)
	}
	return b.Bytes()
}
