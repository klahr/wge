package build

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"time"
)

// historyPools are the commands each kind of account plausibly ran.
//
// Shell history is the cheapest characterisation in the game: it shows what
// somebody was doing and how competent they were, without a word of exposition.
var historyPools = map[Role][]string{
	RolePlain: {
		"ls -la", "cd ~", "df -h", "cat /etc/hostname", "who", "uptime",
		"grep -r TODO .", "vim notes.txt", "less README", "mkdir tmp",
		"cp notes.txt notes.txt.bak", "history | tail -40", "id",
	},
	RoleMail: {
		"mailq", "postqueue -p", "tail -f /var/log/mail.log", "postconf -n",
		"grep 'status=bounced' /var/log/mail.log", "systemctl status postfix",
		"postsuper -d ALL deferred", "cat /etc/postfix/main.cf",
		"grep -c 'relay=' /var/log/mail.log",
	},
	RoleBackup: {
		"df -h /var/backups", "ls -lh /var/backups", "du -sh /var/backups/*",
		"tar tzf /var/backups/nightly.tar.gz | head", "crontab -l",
		"rsync -avn /home/ /var/backups/staging/", "systemctl status backup-agent",
		"journalctl -u backup-agent --since yesterday", "find /var/backups -mtime +30",
	},
	RoleOps: {
		"systemctl --failed", "journalctl -p err -n 50", "ss -tlnp", "ps aux --sort=-%mem",
		"top -b -n1 | head -20", "dmesg | tail", "systemctl restart nginx",
		"free -m", "iostat -x 5 2", "cat /proc/loadavg",
	},
	RoleAdmin: {
		"sudo -l", "visudo -c", "getent passwd | wc -l", "last -20",
		"grep -i 'authentication failure' /var/log/auth.log", "chage -l root",
		"find / -perm -4000 -type f 2>/dev/null", "ss -tlnp", "systemctl list-timers",
	},
}

// unfinished are the commands a history ends on.
//
// A history that stops on a half-typed command is the single most evocative
// thing that can be put in a home directory: somebody was interrupted, and the
// player has walked in afterwards.
var unfinished = []string{
	"grep -r 'forward' /etc/postfix/",
	"tar tzf /var/backups/nightly-",
	"ls -la /home/",
	"sudo -u ",
	"cat /var/log/mail.log | grep -v ",
	"ssh ",
	"find / -name '*.key' -readab",
}

// bashHistory generates a plausible history for an account.
func bashHistory(a *Aging, acct *account, lines int) []byte {
	pool := historyPools[acct.role()]
	if len(pool) == 0 {
		pool = historyPools[RolePlain]
	}

	var b bytes.Buffer
	times := a.Spread("history/"+acct.User, lines)

	for i := 0; i < lines; i++ {
		cmd := pool[int(a.fraction("hist", fmt.Sprintf("%s/%d", acct.User, i))*float64(len(pool)))%len(pool)]

		// Real histories repeat: people run the same check again and again.
		if i > 0 && a.fraction("repeat", fmt.Sprintf("%s/%d", acct.User, i)) < 0.18 {
			cmd = pool[int(a.fraction("hist", fmt.Sprintf("%s/%d", acct.User, i-1))*float64(len(pool)))%len(pool)]
		}

		// HISTTIMEFORMAT records leave timestamp lines interleaved with the
		// commands, which is what a box with a sane profile looks like.
		fmt.Fprintf(&b, "#%d\n%s\n", times[i].Unix(), cmd)
	}

	last := unfinished[int(a.fraction("unfinished", acct.User)*float64(len(unfinished)))%len(unfinished)]
	fmt.Fprintf(&b, "#%d\n%s\n", times[len(times)-1].Add(time.Minute).Unix(), last)

	return b.Bytes()
}

// utmp record layout on 64-bit glibc. The fields are fixed width and the
// timeval is 32-bit even on 64-bit systems, for compatibility with the format
// as it has always been on disk.
const (
	utmpRecordSize = 384
	utmpOffPID     = 4
	utmpOffLine    = 8
	utmpOffID      = 40
	utmpOffUser    = 44
	utmpOffHost    = 76
	utmpOffSession = 336
	utmpOffTime    = 340
	utmpOffAddr    = 348
)

// utmp record types.
const (
	utmpBootTime    = 2
	utmpUserProcess = 7
	utmpDeadProcess = 8
)

// utmpRecord renders one wtmp entry.
func utmpRecord(kind int16, pid int32, line, id, user, host string, at time.Time) []byte {
	rec := make([]byte, utmpRecordSize)

	binary.LittleEndian.PutUint16(rec[0:], uint16(kind))
	binary.LittleEndian.PutUint32(rec[utmpOffPID:], uint32(pid))
	copyFixed(rec[utmpOffLine:utmpOffID], line)
	copyFixed(rec[utmpOffID:utmpOffUser], id)
	copyFixed(rec[utmpOffUser:utmpOffHost], user)
	copyFixed(rec[utmpOffHost:utmpOffHost+256], host)
	binary.LittleEndian.PutUint32(rec[utmpOffSession:], 0)
	binary.LittleEndian.PutUint32(rec[utmpOffTime:], uint32(at.Unix()))
	binary.LittleEndian.PutUint32(rec[utmpOffTime+4:], 0)

	// A login from somewhere with no address recorded is normal; leaving the
	// address zeroed matches what a console or su session looks like.
	binary.LittleEndian.PutUint32(rec[utmpOffAddr:], 0)

	return rec
}

func copyFixed(dst []byte, s string) {
	if len(s) >= len(dst) {
		s = s[:len(dst)-1]
	}
	copy(dst, s)
}

// loginHosts are the addresses sessions appear to have come from.
var loginHosts = []string{
	"10.14.2.31", "10.14.2.44", "10.14.6.9", "vpn-gw.ardent.internal",
	"10.14.2.18", "sundqvist-lt.ardent.internal", "10.14.7.102",
}

// wtmp renders the session list into the classic binary login log.
//
// last(1) reporting "wtmp begins" at the image build time, with no sessions
// before it, says plainly that the machine was created moments ago. A box that
// people have logged into for months is a box that has been in service.
func wtmp(a *Aging, sessions []session) []byte {
	type record struct {
		at  time.Time
		rec []byte
	}
	var records []record

	// The boot record anchors what last(1) calls the beginning of wtmp.
	boot := a.Start()
	records = append(records, record{boot,
		utmpRecord(utmpBootTime, 0, "~", "~~", "reboot", "7.1.0-generic", boot)})

	for i, s := range sessions {
		id := fmt.Sprintf("ts%02d", i%100)
		records = append(records,
			record{s.Login, utmpRecord(utmpUserProcess, int32(s.PID), s.TTY, id, s.User, s.Host, s.Login)},
			record{s.Logout, utmpRecord(utmpDeadProcess, int32(s.PID), s.TTY, id, "", "", s.Logout)})
	}

	// wtmp is append-ordered, so it has to be written in time order or last(1)
	// reports nonsense.
	sort.Slice(records, func(i, j int) bool { return records[i].at.Before(records[j].at) })

	var b bytes.Buffer
	for _, r := range records {
		b.Write(r.rec)
	}
	return b.Bytes()
}

// lastlog record layout: a fixed-size record per uid, indexed by uid.
const (
	lastlogRecordSize = 292
	lastlogOffLine    = 4
	lastlogOffHost    = 36
)

// lastlog renders the classic per-account last-login database.
//
// The file is indexed by uid, so it is as large as the highest uid on the box.
// That is how the real one behaves: it is a sparse file, and its apparent size
// is not its cost on disk.
func lastlog(accounts []*account, sessions []session) []byte {
	highest := 0
	for _, acct := range accounts {
		if acct.UID > highest {
			highest = acct.UID
		}
	}

	buf := make([]byte, (highest+1)*lastlogRecordSize)
	byUser := map[string]*account{}
	for _, acct := range accounts {
		byUser[acct.User] = acct
	}

	for user, s := range latestPerUser(sessions) {
		acct, ok := byUser[user]
		if !ok {
			continue
		}
		rec := buf[acct.UID*lastlogRecordSize : (acct.UID+1)*lastlogRecordSize]
		binary.LittleEndian.PutUint32(rec[0:], uint32(s.Login.Unix()))
		copyFixed(rec[lastlogOffLine:lastlogOffHost], s.TTY)
		copyFixed(rec[lastlogOffHost:], s.Host)
	}
	return buf
}

// authLog renders the same sessions as sshd would have logged them, plus the
// failures and sudo calls that go with them.
func authLog(a *Aging, accounts []*account, sessions []session, host string) []byte {
	roles := map[string]Role{}
	uids := map[string]int{}
	for _, acct := range accounts {
		roles[acct.User] = acct.role()
		uids[acct.User] = acct.UID
	}

	type line struct {
		at   time.Time
		text string
	}
	var lines []line

	for i, s := range sessions {
		key := fmt.Sprintf("%s/%d", s.User, i)

		lines = append(lines, line{s.Login, fmt.Sprintf(
			"sshd[%d]: Accepted publickey for %s from %s port %d ssh2: ED25519 SHA256:%s",
			s.PID, s.User, s.Host, 40000+s.PID%20000, fakeFingerprint(a, s.User))})
		lines = append(lines, line{s.Login.Add(time.Second), fmt.Sprintf(
			"sshd[%d]: pam_unix(sshd:session): session opened for user %s(uid=%d) by (uid=0)",
			s.PID, s.User, uids[s.User])})
		lines = append(lines, line{s.Logout, fmt.Sprintf(
			"sshd[%d]: pam_unix(sshd:session): session closed for user %s", s.PID, s.User)})

		// Wrong passwords happen to everyone; a log with none is a log nobody
		// wrote.
		if a.fraction("failed", key) < 0.22 {
			lines = append(lines, line{s.Login.Add(-90 * time.Second), fmt.Sprintf(
				"sshd[%d]: Failed password for %s from %s port %d ssh2",
				s.PID-3, s.User, s.Host, 40000+s.PID%20000)})
		}
		if roles[s.User] == RoleAdmin && a.fraction("sudo", key) < 0.5 {
			lines = append(lines, line{s.Login.Add(4 * time.Minute), fmt.Sprintf(
				"sudo:  %s : TTY=%s ; PWD=/home/%s ; USER=root ; COMMAND=/usr/bin/systemctl status postfix",
				s.User, s.TTY, s.User)})
		}
	}

	sort.Slice(lines, func(i, j int) bool { return lines[i].at.Before(lines[j].at) })

	var b bytes.Buffer
	for _, l := range lines {
		// syslog space-pads the day to two columns: "Jan  6", "Jan 16". Go's
		// _2 is that padding; a literal double space would give "Jan  16".
		fmt.Fprintf(&b, "%s %s %s\n", l.at.Format("Jan _2 15:04:05"), host, l.text)
	}
	return b.Bytes()
}

func fakeFingerprint(a *Aging, key string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var b strings.Builder
	for i := 0; i < 43; i++ {
		f := a.fraction("fp", fmt.Sprintf("%s/%d", key, i))
		b.WriteByte(alphabet[int(f*float64(len(alphabet)))%len(alphabet)])
	}
	return b.String()
}

// mailbox renders an mbox file from a set of messages.
func mailbox(messages []string) []byte {
	var b bytes.Buffer
	for _, m := range messages {
		if !strings.HasPrefix(m, "From ") {
			// mbox requires each message to start with a From_ line; authors
			// write plain RFC 822 and the engine supplies the envelope.
			b.WriteString("From MAILER-DAEMON " + time.Time{}.Format(time.ANSIC) + "\n")
		}
		b.WriteString(strings.TrimRight(m, "\n"))
		b.WriteString("\n\n")
	}
	return b.Bytes()
}
