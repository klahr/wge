package manifest

import (
	"strings"
	"testing"
	"time"
)

// valid returns a small but complete game: a chain that forks and rejoins, with
// the rejoin gated on a split credential. Each test mutates one thing.
func valid() *Game {
	return &Game{
		ID:       "heist",
		Version:  1,
		Title:    "The Mailroom Job",
		Base:     "wge/debian-13",
		Timeline: Timeline{Start: time.Date(2024, 11, 3, 9, 0, 0, 0, time.UTC)},
		Levels: []*Level{
			{
				ID: "mailroom", User: "jposti", Host: DefaultHostID,
				Grants: []Grant{{Kind: GrantPassword, To: "backup-op", PlacedIn: "/var/mail/jposti"}},
			},
			{
				ID: "backup-op", User: "bkup", Host: DefaultHostID,
				Requires: []string{"mailroom"},
				Grants: []Grant{
					{Kind: GrantPassword, To: "ops-oncall", PlacedIn: "/var/backups/rota.db"},
					{Kind: GrantSSHKey, To: "sysadmin", PlacedIn: "/var/backups/restored/home/d/.ssh/id_ed25519"},
				},
			},
			{
				ID: "ops-oncall", User: "oncall", Host: DefaultHostID,
				Requires: []string{"backup-op"},
				Grants:   []Grant{{Kind: GrantPassphrase, To: "sysadmin", PlacedIn: "/home/oncall/.local/share/notes.txt"}},
			},
			{
				ID: "sysadmin", User: "dsundqvist", Host: DefaultHostID,
				Requires: []string{"backup-op", "ops-oncall"},
			},
		},
	}
}

func mustFail(t *testing.T, g *Game, want string) {
	t.Helper()
	err := g.Validate()
	if err == nil {
		t.Fatalf("expected validation to fail with %q, got no error", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("expected an error containing %q, got:\n%s", want, err)
	}
}

func TestValidGamePasses(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("the reference game should validate: %v", err)
	}
}

// The check that catches the most common authoring mistake: a declared
// prerequisite that never actually hands anything over.
func TestPrerequisiteThatGrantsNothing(t *testing.T) {
	g := valid()
	g.Levels[0].Grants = nil // mailroom no longer opens backup-op
	mustFail(t, g, "requires mailroom, but mailroom grants it nothing")
}

// And its mirror: a credential handed over along an edge the graph never claims.
func TestGrantWithoutDeclaredPrerequisite(t *testing.T) {
	g := valid()
	g.Levels[3].Requires = []string{"backup-op"} // sysadmin forgets ops-oncall
	mustFail(t, g, "does not list it as a prerequisite")
}

func TestSplitCredentialMustBeDistinctKinds(t *testing.T) {
	g := valid()
	// Both parents now hand over a key; either alone would do, so the second
	// prerequisite is not really required.
	g.Levels[2].Grants[0].Kind = GrantSSHKey
	mustFail(t, g, "receives a ssh-key from both")
}

func TestPassphraseWithoutKeyOpensNothing(t *testing.T) {
	g := valid()
	g.Levels[1].Grants[1].Kind = GrantPassphrase // sysadmin gets two passphrases...
	g.Levels[2].Grants[0].Kind = GrantPassphrase
	mustFail(t, g, "passphrase opens nothing")
}

func TestLevelWithNoWayIn(t *testing.T) {
	g := valid()
	g.Levels[1].Grants[1] = Grant{Kind: GrantPassphrase, To: "sysadmin", PlacedIn: "/tmp/p"}
	g.Levels[2].Grants[0] = Grant{Kind: GrantSSHKey, To: "sysadmin", PlacedIn: "/tmp/k"}
	if err := g.Validate(); err != nil {
		t.Fatalf("key plus passphrase is a valid way in: %v", err)
	}

	// Remove the key and only the passphrase remains.
	g.Levels[2].Grants = nil
	g.Levels[3].Requires = []string{"backup-op"}
	mustFail(t, g, "no password and no key")
}

func TestCycleIsReportedWithItsPath(t *testing.T) {
	g := valid()
	g.Levels[0].Requires = []string{"sysadmin"} // mailroom now depends on the end
	err := g.Validate()
	if err == nil {
		t.Fatal("expected a cycle to be rejected")
	}
	if !strings.Contains(err.Error(), "prerequisite cycle") {
		t.Fatalf("expected a cycle report, got:\n%s", err)
	}
	// The path is what makes the error actionable.
	if !strings.Contains(err.Error(), "->") {
		t.Fatalf("cycle error should name the path, got:\n%s", err)
	}
}

func TestExactlyOneEntryLevel(t *testing.T) {
	g := valid()
	g.Levels[2].Requires = nil // ops-oncall becomes a second starting point
	mustFail(t, g, "exactly one entry level")

	g = valid()
	for _, l := range g.Levels {
		if len(l.Requires) == 0 {
			l.Requires = []string{"sysadmin"}
		}
	}
	mustFail(t, g, "no entry level")
}

func TestUnreachableLevel(t *testing.T) {
	g := valid()
	g.Levels = append(g.Levels, &Level{
		ID: "orphan", User: "nobody2", Host: DefaultHostID,
		Requires: []string{"mailroom"},
	})
	// mailroom does not grant to orphan, so it is both ungranted and unreachable.
	mustFail(t, g, "unreachable from mailroom")
}

func TestGameMustEndSomewhere(t *testing.T) {
	g := valid()
	g.Levels[3].Grants = []Grant{{Kind: GrantPassword, To: "mailroom", PlacedIn: "/tmp/x"}}
	g.Levels[0].Requires = []string{"sysadmin"}
	mustFail(t, g, "no final level")
}

func TestTwoLevelsCannotShareAnAccount(t *testing.T) {
	g := valid()
	g.Levels[2].User = "bkup"
	mustFail(t, g, "both use the account")
}

func TestTimelineIsRequiredForAging(t *testing.T) {
	g := valid()
	g.Timeline.Start = time.Time{}
	mustFail(t, g, "timeline.start is required")
}

func TestGrantMustSayWhereTheCredentialIs(t *testing.T) {
	g := valid()
	g.Levels[0].Grants[0].PlacedIn = ""
	mustFail(t, g, "does not say where the credential is placed")

	g = valid()
	g.Levels[0].Grants[0].PlacedIn = "var/mail/jposti"
	mustFail(t, g, "relative path")
}

// Rendering a per-run credential inside an archive would mean repacking it
// during seeding. Until the pipeline can, saying so beats producing a game
// whose credential never appears.
func TestArchiveMemberGrantIsRejected(t *testing.T) {
	g := valid()
	g.Levels[1].Grants[1].PlacedIn = "/var/backups/nightly.tar.gz:home/d/.ssh/id_ed25519"
	mustFail(t, g, "credentials inside archives are not supported yet")
}

func TestServiceMayNotRunAsRoot(t *testing.T) {
	g := valid()
	g.Levels[1].Services = []Service{{Name: "backup-agent", User: "root", Port: 8377}}
	mustFail(t, g, "runs as root")
}

// A service sharing a level's account merges two nodes of the permission graph
// without the graph saying so: compromising the service silently solves a level.
func TestServiceMayNotShareALevelAccount(t *testing.T) {
	g := valid()
	g.Levels[1].Services = []Service{{Name: "backup-agent", User: "dsundqvist", Port: 8377}}
	mustFail(t, g, "which is level sysadmin's account")
}

func TestUnknownHostIsRejected(t *testing.T) {
	g := valid()
	g.Hosts = []*Host{{ID: "mailsrv", Base: "wge/debian-13"}}
	g.Levels[0].Host = "nowhere"
	mustFail(t, g, `unknown host "nowhere"`)
}

func TestHostEgressMustNameKnownHosts(t *testing.T) {
	g := valid()
	g.Hosts = []*Host{
		{ID: "mailsrv", Base: "wge/debian-13", Egress: []string{"vault"}},
	}
	for _, l := range g.Levels {
		l.Host = "mailsrv"
	}
	mustFail(t, g, `egress to unknown host "vault"`)
}

func TestInvalidIdentifiers(t *testing.T) {
	g := valid()
	g.Levels[0].User = "Not A User"
	mustFail(t, g, "not a valid Unix username")

	g = valid()
	g.Levels[0].ID = "Mailroom"
	mustFail(t, g, "must be a lowercase slug")
}

// All faults in one pass: authors should not have to fix these one build at a time.
func TestErrorsAreAccumulated(t *testing.T) {
	g := valid()
	g.Title = ""
	g.Timeline.Start = time.Time{}
	g.Levels[0].User = "Not A User"

	err := g.Validate()
	if err == nil {
		t.Fatal("expected failures")
	}
	var errs Errors
	if !asErrors(err, &errs) {
		t.Fatalf("expected manifest.Errors, got %T", err)
	}
	if len(errs) < 3 {
		t.Fatalf("expected at least 3 accumulated problems, got %d:\n%s", len(errs), err)
	}
}

func asErrors(err error, out *Errors) bool {
	e, ok := err.(Errors)
	if ok {
		*out = e
	}
	return ok
}

func TestGrantPathSplitting(t *testing.T) {
	g := Grant{PlacedIn: "/var/backups/n.tar.gz:home/d/.ssh/id_ed25519"}
	if got := g.Path(); got != "/var/backups/n.tar.gz" {
		t.Errorf("Path() = %q", got)
	}
	if got := g.Member(); got != "home/d/.ssh/id_ed25519" {
		t.Errorf("Member() = %q", got)
	}

	plain := Grant{PlacedIn: "/var/mail/jposti"}
	if got := plain.Path(); got != "/var/mail/jposti" {
		t.Errorf("Path() = %q", got)
	}
	if got := plain.Member(); got != "" {
		t.Errorf("Member() = %q, want empty", got)
	}
}
