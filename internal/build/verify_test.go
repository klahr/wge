package build

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klahr/wge/internal/docker"
	"github.com/klahr/wge/internal/manifest"
)

// The verifier's whole value is that it fails when something is wrong. These
// tests build deliberately broken images and assert each check fires, because
// a permission check that cannot detect a leak is worse than none: it says the
// game is safe to serve.

const verifyBase = "wge/debian-13"

func requireDocker(t *testing.T) *docker.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("builds container images; skipped in short mode")
	}

	api := docker.New("")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var info struct{ ID string }
	if err := api.Get(ctx, "/images/"+verifyBase+"/json", &info); err != nil {
		unavailable(t, "base image %s is not available: %v", verifyBase, err)
	}
	return api
}

// unavailable skips, or fails when the environment has promised Docker.
//
// These tests skip themselves when there is no engine to talk to, which is
// right on a laptop and wrong in CI: a run that skipped everything reports the
// same green as a run that proved something. WGE_REQUIRE_DOCKER says the
// engine and its base image are supposed to be here, and turns the skip into
// the failure it should be.
func unavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("WGE_REQUIRE_DOCKER") != "" {
		t.Fatalf("WGE_REQUIRE_DOCKER is set but "+format, args...)
	}
	t.Skipf(format, args...)
}

// writeGame lays out a three-level chain. Three is the minimum that gives a
// level a credential it must not be able to see: the first level may know the
// second's password, because handing it over is what the level is for, but it
// must never see the third's.
func writeGame(t *testing.T, id string, setup map[string]string) string {
	t.Helper()
	dir := t.TempDir()

	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("game.yaml", fmt.Sprintf(`id: %s
version: 1
title: Verification Fixture
base: %s
staff:
  - user: rhelin
    name: Reeta Helin
  - user: kmakela
    name: Kalle Makela
timeline:
  start: 2024-11-03T09:00:00Z
`, id, verifyBase))

	write("levels/01-alpha/level.yaml", `id: alpha
user: alpha
requires: []
grants:
  - kind: password
    to: bravo
    placed_in: /home/alpha/handover.txt
`)
	write("levels/01-alpha/home/handover.txt", `next account: {{ .Password "bravo" }}`)

	write("levels/02-bravo/level.yaml", `id: bravo
user: bravo
requires: [alpha]
grants:
  - kind: password
    to: charlie
    placed_in: /home/bravo/notes.txt
`)
	write("levels/02-bravo/home/notes.txt", `final account: {{ .Password "charlie" }}`)

	write("levels/03-charlie/level.yaml", `id: charlie
user: charlie
requires: [bravo]
grants: []
`)
	write("levels/03-charlie/home/done.txt", "the end")

	for level, script := range setup {
		write(filepath.Join("levels", level, "setup.sh"), "#!/bin/sh\nset -eu\n"+script)
	}
	return dir
}

// verifyFixture builds a fixture and returns the findings.
func verifyFixture(t *testing.T, id string, setup map[string]string) []Finding {
	t.Helper()
	return verifyDir(t, id, writeGame(t, id, setup))
}

// verifyDir builds and verifies a game that is already laid out.
func verifyDir(t *testing.T, id, dir string) []Finding {
	t.Helper()
	api := requireDocker(t)
	ctx := context.Background()

	g, err := manifest.Load(dir)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	image := "wge-test/" + id + ":1-main"
	t.Cleanup(func() { _ = api.RemoveImage(context.Background(), image) })

	if _, err := NewBuilder("").Build(ctx, g, manifest.DefaultHostID, image, nil); err != nil {
		t.Fatalf("build fixture: %v", err)
	}

	report, err := NewVerifier("").Verify(ctx, g, manifest.DefaultHostID, image)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.Checks == 0 {
		t.Fatal("the verifier ran no checks")
	}
	return report.Findings
}

func findingsOfKind(findings []Finding, kind FindingKind) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

// A correctly built game must come back clean, or every other result here is
// noise.
func TestVerifyAcceptsAWellBuiltGame(t *testing.T) {
	findings := verifyFixture(t, "clean", map[string]string{
		"01-alpha": "chmod 0600 /home/alpha/handover.txt",
		"02-bravo": "chmod 0600 /home/bravo/notes.txt",
	})
	if len(findings) != 0 {
		t.Fatalf("a well built game reported %d problems:\n%v", len(findings), findings)
	}
}

// The failure that ends a game: a credential readable by a level that has not
// earned it short-circuits the graph regardless of what the graph says.
func TestVerifyCatchesALeakedCredential(t *testing.T) {
	findings := verifyFixture(t, "leaky", map[string]string{
		"01-alpha": "chmod 0600 /home/alpha/handover.txt",
		// bravo's notes hold charlie's password, and alpha has no business
		// reading them.
		"02-bravo": "chmod 0755 /home/bravo\nchmod 0644 /home/bravo/notes.txt",
	})

	leaks := findingsOfKind(findings, FindingCredentialLeak)
	if len(leaks) == 0 {
		t.Fatalf("a world-readable credential was not detected; findings: %v", findings)
	}

	var found bool
	for _, f := range leaks {
		if f.Level == "alpha" && f.Path == "/home/bravo/notes.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected alpha to be caught reading /home/bravo/notes.txt, got %v", leaks)
	}

	// The same file is also somebody else's property, so the ownership check
	// should independently object.
	if len(findingsOfKind(findings, FindingLeak)) == 0 {
		t.Error("the file ownership check did not object to the same file")
	}
}

// The mirror failure: a credential the level cannot reach makes everything
// after it unreachable.
func TestVerifyCatchesAnUnreachableCredential(t *testing.T) {
	findings := verifyFixture(t, "unsolvable", map[string]string{
		"01-alpha": "chmod 0000 /home/alpha/handover.txt",
		"02-bravo": "chmod 0600 /home/bravo/notes.txt",
	})

	unsolvable := findingsOfKind(findings, FindingUnsolvable)
	if len(unsolvable) == 0 {
		t.Fatalf("an unreadable credential was not detected; findings: %v", findings)
	}
	if unsolvable[0].Level != "alpha" || unsolvable[0].Path != "/home/alpha/handover.txt" {
		t.Errorf("unexpected finding: %v", unsolvable[0])
	}
}

// find / -perm -4000 is the first thing a competent player runs, and it should
// return exactly what the author intended.
func TestVerifyCatchesAnUnexpectedSetuidBinary(t *testing.T) {
	findings := verifyFixture(t, "setuid", map[string]string{
		"01-alpha": "chmod 0600 /home/alpha/handover.txt\n" +
			"cp /bin/sh /usr/local/bin/backup-helper\n" +
			"chmod 4755 /usr/local/bin/backup-helper",
		"02-bravo": "chmod 0600 /home/bravo/notes.txt",
	})

	setuid := findingsOfKind(findings, FindingSetuid)
	if len(setuid) == 0 {
		t.Fatalf("an added setuid binary was not detected; findings: %v", findings)
	}
	if setuid[0].Path != "/usr/local/bin/backup-helper" {
		t.Errorf("unexpected finding: %v", setuid[0])
	}
}

// writeArchiveGame puts the second credential inside an archive, and leaves the
// archive readable by everybody.
//
// The credential sweep cannot catch this: it greps files, and a gzip is not
// text. Only the manifest knows what is inside, which is what the archive
// check uses.
func writeArchiveGame(t *testing.T, id string, mode string) string {
	t.Helper()
	dir := writeGame(t, id, map[string]string{
		"01-alpha": "chmod 0600 /home/alpha/handover.txt",
		// /srv is a system directory, so listing it is nobody's leak: what is
		// under test is the archive file's own mode.
		"02-bravo": "chmod " + mode + " /srv/nightly.tar.gz",
	})

	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// bravo's credential for charlie moves inside a tarball.
	write("levels/02-bravo/level.yaml", `id: bravo
user: bravo
name: Bravo Account
requires: [alpha]
grants:
  - kind: password
    to: charlie
    placed_in: /srv/nightly.tar.gz:secrets/handover.txt
`)
	if err := os.Remove(filepath.Join(dir, "levels/02-bravo/home/notes.txt")); err != nil {
		t.Fatal(err)
	}
	write("levels/02-bravo/files/srv/nightly.tar.gz/secrets/handover.txt",
		`final account: {{ .Password "charlie" }}`)
	return dir
}

// An archive only its owner can open is fine.
func TestArchiveCredentialIsAcceptedWhenTheArchiveIsClosed(t *testing.T) {
	findings := verifyDir(t, "arcclosed", writeArchiveGame(t, "arcclosed", "0600"))
	if len(findings) != 0 {
		t.Fatalf("a closed archive reported %d problems:\n%v", len(findings), findings)
	}
}

// A world-readable one hands its contents to whoever can open it, whatever the
// permissions on the file the credential would otherwise have been in.
func TestArchiveCredentialLeaksWhenTheArchiveIsOpen(t *testing.T) {
	findings := verifyDir(t, "arcopen", writeArchiveGame(t, "arcopen", "0644"))

	leaks := findingsOfKind(findings, FindingCredentialLeak)
	if len(leaks) == 0 {
		t.Fatalf("a readable archive holding a credential was not detected; findings: %v", findings)
	}

	var found bool
	for _, f := range leaks {
		if f.Level == "alpha" && f.Path == "/srv/nightly.tar.gz" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected alpha to be caught opening the archive, got %v", leaks)
	}
}
