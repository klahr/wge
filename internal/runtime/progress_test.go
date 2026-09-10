package runtime

import (
	"reflect"
	"strings"
	"testing"
)

func TestParsesSessionsOpenedByBothRoutes(t *testing.T) {
	log := strings.Join([]string{
		`Sep 10 05:53:20 vault sshd-session[1225]: Accepted publickey for dsundqvist from 172.22.0.2 port 59386 ssh2: ED25519 SHA256:abc`,
		`Sep 10 05:53:20 vault sshd-session[1225]: pam_unix(sshd:session): session opened for user dsundqvist(uid=1301) by (uid=0)`,
		`Sep 10 05:53:27 relay2 su[1309]: pam_unix(su:session): session opened for user bkup(uid=1167) by (uid=1084)`,
		`Sep 10 05:53:27 vault sshd-session[1225]: pam_unix(sshd:session): session closed for user dsundqvist`,
	}, "\n")

	got := parseSessions(log)
	want := []string{"dsundqvist", "bkup"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSessions = %v, want %v", got, want)
	}
}

// A player passing through a level several times is one arrival.
func TestSessionsAreDeduplicated(t *testing.T) {
	log := strings.Repeat(
		"Sep 10 05:53:27 relay2 su[1]: pam_unix(su:session): session opened for user bkup(uid=1)\n", 5)

	if got := parseSessions(log); !reflect.DeepEqual(got, []string{"bkup"}) {
		t.Fatalf("parseSessions = %v, want one entry", got)
	}
}

// A session closing is not a session opening, and neither is a failure.
func TestOnlyOpenedSessionsCount(t *testing.T) {
	log := strings.Join([]string{
		`Sep 10 05:53:27 relay2 su[1]: pam_unix(su:session): session closed for user bkup`,
		`Sep 10 05:53:28 relay2 su[2]: pam_authenticate: Authentication failure`,
		`Sep 10 05:53:29 relay2 sshd[3]: Failed password for oncall from 10.14.2.31 port 40000 ssh2`,
		`Sep 10 05:53:30 relay2 sshd[4]: Invalid user attacker from 10.0.0.1`,
	}, "\n")

	if got := parseSessions(log); len(got) != 0 {
		t.Fatalf("parseSessions = %v, want nothing", got)
	}
}

// This is why the offset is load-bearing rather than a shortcut.
//
// The history the aging pass writes uses exactly the format a live session
// uses -- that is the whole point of it -- so no amount of parsing can tell
// the two apart. Only knowing where the file had got to when the machine
// finished booting can.
func TestGeneratedHistoryIsIndistinguishableFromLiveSessions(t *testing.T) {
	history := `Aug 26 02:17:00 relay2 sshd[2289]: pam_unix(sshd:session): session opened for user jposti(uid=1084) by (uid=0)`

	if got := parseSessions(history); !reflect.DeepEqual(got, []string{"jposti"}) {
		t.Fatalf("the generated history parses as %v; if it did not, the offset "+
			"would be unnecessary and the history would be detectable", got)
	}
}

func TestNonLevelAccountsAreStillParsed(t *testing.T) {
	// Filtering to levels happens against the game, not in the parser: a
	// decoy's session is a real session, it just is not a level.
	log := `Sep 10 05:53:27 relay2 sshd[1]: pam_unix(sshd:session): session opened for user aringdal(uid=1400) by (uid=0)`

	if got := parseSessions(log); !reflect.DeepEqual(got, []string{"aringdal"}) {
		t.Fatalf("parseSessions = %v", got)
	}
}
