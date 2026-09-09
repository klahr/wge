package build

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func testAccounts(t *testing.T) []*account {
	t.Helper()
	return []*account{
		{User: "jposti", UID: 1084, GID: 1084, Home: "/home/jposti"},
		{User: "bkup", UID: 1201, GID: 1201, Home: "/home/bkup"},
		{User: "backupd", UID: 160, GID: 160, Home: "/var/lib/backupd", Service: true},
	}
}

// syslog space-pads the day to two columns. A malformed date is the kind of
// detail a player reads past a hundred times and notices once.
func TestAuthLogUsesSyslogDateFormat(t *testing.T) {
	a := testAging()
	sessions := a.sessions(testAccounts(t))
	log := string(authLog(a, testAccounts(t), sessions, "relay2"))

	valid := regexp.MustCompile(`^[A-Z][a-z]{2} [ 0-9][0-9] \d{2}:\d{2}:\d{2} relay2 `)
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if len(lines) < 5 {
		t.Fatalf("auth.log has only %d lines", len(lines))
	}
	for _, line := range lines {
		if !valid.MatchString(line) {
			t.Fatalf("malformed syslog line: %q", line)
		}
	}
}

// A player who cross-checks last against auth.log and finds two different
// stories has learned more from the discrepancy than from either.
func TestLoginRecordsAgreeWithAuthLog(t *testing.T) {
	a := testAging()
	accounts := testAccounts(t)
	sessions := a.sessions(accounts)
	log := string(authLog(a, accounts, sessions, "relay2"))

	for _, s := range sessions {
		opened := s.Login.Add(time.Second).Format("Jan _2 15:04:05")
		if !strings.Contains(log, opened) {
			t.Errorf("session for %s at %s has no matching auth.log entry", s.User, opened)
		}
	}
}

// Service accounts have nologin shells. A daemon account with a login history
// is a contradiction a player can spot.
func TestServiceAccountsNeverLogIn(t *testing.T) {
	a := testAging()
	for _, s := range a.sessions(testAccounts(t)) {
		if s.User == "backupd" {
			t.Fatal("a nologin service account has a login session")
		}
	}
}

func TestLoginDatabasesAreGenerated(t *testing.T) {
	a := testAging()
	accounts := testAccounts(t)
	sessions := a.sessions(accounts)

	wtmpdb, err := wtmpdbDatabase(a, sessions)
	if err != nil {
		t.Fatalf("wtmpdbDatabase: %v", err)
	}
	if !strings.HasPrefix(string(wtmpdb), "SQLite format 3") {
		t.Error("wtmpdb output is not a SQLite database")
	}

	lastlog2, err := lastlog2Database(sessions)
	if err != nil {
		t.Fatalf("lastlog2Database: %v", err)
	}
	if !strings.HasPrefix(string(lastlog2), "SQLite format 3") {
		t.Error("lastlog2 output is not a SQLite database")
	}

	// The binary pair is still written for images built on older bases.
	binary := wtmp(a, sessions)
	if len(binary)%utmpRecordSize != 0 {
		t.Errorf("wtmp is %d bytes, not a whole number of %d-byte records", len(binary), utmpRecordSize)
	}
	if len(binary) < utmpRecordSize*(len(sessions)*2+1) {
		t.Error("wtmp is missing records")
	}
}

// A shell history that stops on a half-typed command is the most evocative
// thing that can be left in a home directory.
func TestHistoryEndsMidThought(t *testing.T) {
	a := testAging()
	acct := &account{User: "bkup", UID: 1201}

	lines := strings.Split(strings.TrimRight(string(bashHistory(a, acct, 20)), "\n"), "\n")
	last := lines[len(lines)-1]

	for _, complete := range historyPools[acct.role()] {
		if last == complete {
			t.Fatalf("history ends on a complete command %q", last)
		}
	}
	if !strings.HasPrefix(lines[0], "#") {
		t.Error("history is missing its timestamp lines")
	}
}
