package build

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

// session is one login the box remembers.
//
// Every artifact that records logins -- wtmp, wtmpdb, lastlog, lastlog2 and
// auth.log -- is rendered from this one list, so they agree with each other. A
// player who cross-checks `last` against /var/log/auth.log and finds two
// different stories has learned more from the discrepancy than from either.
type session struct {
	User    string
	TTY     string
	Host    string
	PID     int
	Login   time.Time
	Logout  time.Time
	Service string
}

// sessions invents the box's login history.
func (a *Aging) sessions(accounts []*account) []session {
	var out []session
	tty := 0

	for _, acct := range accounts {
		// Service accounts have nologin shells; they never log in, which is
		// the point of them.
		if acct.Service {
			continue
		}

		count := 3 + int(a.fraction("sessions", acct.User)*9)
		times := a.Spread("login/"+acct.User, count)

		for i, at := range times {
			tty++
			duration := time.Duration(scale(
				a.fraction("duration", fmt.Sprintf("%s/%d", acct.User, i)),
				float64(4*time.Minute), float64(7*time.Hour)))

			out = append(out, session{
				User:    acct.User,
				TTY:     fmt.Sprintf("pts/%d", tty%16),
				Host:    loginHosts[int(a.fraction("from", fmt.Sprintf("%s/%d", acct.User, i))*float64(len(loginHosts)))%len(loginHosts)],
				PID:     2000 + tty*17,
				Login:   at,
				Logout:  at.Add(duration),
				Service: "sshd",
			})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Login.Before(out[j].Login) })
	return out
}

// latestPerUser returns each account's most recent session.
func latestPerUser(sessions []session) map[string]session {
	latest := map[string]session{}
	for _, s := range sessions {
		if prior, seen := latest[s.User]; !seen || s.Login.After(prior.Login) {
			latest[s.User] = s
		}
	}
	return latest
}

// Debian 13 replaced the binary login databases with SQLite ones: wtmpdb for
// the session log and lastlog2 for the per-account last login. The binary files
// still exist and are still written by some tooling, so the engine produces
// both -- an image built on an older base reads the binary ones, and a modern
// one reads these.
const (
	// The database itself lives under /var/log. Debian ships a tmpfiles rule
	// making /var/lib/wtmpdb/wtmp.db a symlink to it, and wtmpdb reads the
	// /var/log path directly, so the image needs both to look like a machine
	// where that rule has run.
	wtmpdbPath     = "/var/log/wtmp.db"
	wtmpdbLinkPath = "/var/lib/wtmpdb/wtmp.db"
	wtmpdbLinkDest = "../../log/wtmp.db"

	lastlog2Path = "/var/lib/lastlog/lastlog2.db"
)

// wtmpdb record types, as wtmpdb itself writes them.
const (
	wtmpdbBoot        = 1
	wtmpdbUserProcess = 2
)

// wtmpdbDatabase renders the session log as a wtmpdb database.
func wtmpdbDatabase(a *Aging, sessions []session) ([]byte, error) {
	return buildSQLite(func(db *sql.DB) error {
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS wtmp(
			ID INTEGER PRIMARY KEY, Type INTEGER, User TEXT NOT NULL,
			Login INTEGER, Logout INTEGER, TTY TEXT, RemoteHost TEXT, Service TEXT
		) STRICT`); err != nil {
			return err
		}

		insert := `INSERT INTO wtmp(Type, User, Login, Logout, TTY, RemoteHost, Service)
		           VALUES(?, ?, ?, ?, ?, ?, ?)`

		// The boot record is what `wtmpdb last` calls the beginning of the log.
		// Its logout is null because the machine is still up.
		if _, err := db.Exec(insert, wtmpdbBoot, "reboot",
			a.Start().UnixMicro(), nil, "~", "7.1.0-generic", nil); err != nil {
			return err
		}

		for _, s := range sessions {
			// A user session with no logout renders as an error rather than as
			// "still logged in", so every session here is a closed one.
			if _, err := db.Exec(insert, wtmpdbUserProcess, s.User,
				s.Login.UnixMicro(), s.Logout.UnixMicro(),
				s.TTY, s.Host, s.Service); err != nil {
				return err
			}
		}
		return nil
	})
}

// lastlog2Database renders each account's most recent login.
func lastlog2Database(sessions []session) ([]byte, error) {
	return buildSQLite(func(db *sql.DB) error {
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS Lastlog2(
			Name TEXT PRIMARY KEY, Time INT, TTY TEXT, RemoteHost TEXT, Service TEXT
		)`); err != nil {
			return err
		}

		latest := latestPerUser(sessions)
		users := make([]string, 0, len(latest))
		for user := range latest {
			users = append(users, user)
		}
		sort.Strings(users)

		for _, user := range users {
			s := latest[user]
			if _, err := db.Exec(
				`INSERT INTO Lastlog2(Name, Time, TTY, RemoteHost, Service) VALUES(?, ?, ?, ?, ?)`,
				s.User, s.Login.Unix(), s.TTY, s.Host, s.Service); err != nil {
				return err
			}
		}
		return nil
	})
}

// buildSQLite runs fill against a fresh database and returns the file's bytes.
//
// SQLite needs a real file, so one is written to a temporary directory and read
// back. The result is shipped in the image like any other generated file.
func buildSQLite(fill func(*sql.DB) error) ([]byte, error) {
	dir, err := os.MkdirTemp("", "wge-sqlite-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "out.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	// Journal mode matters: WAL would leave the last writes in a sidecar file
	// that is not part of what gets shipped.
	if _, err := db.Exec(`PRAGMA journal_mode = DELETE`); err != nil {
		db.Close()
		return nil, err
	}
	if err := fill(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}

	return os.ReadFile(path)
}
