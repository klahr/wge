package build

import (
	"fmt"
	"testing"
	"time"
)

var storyStart = time.Date(2024, 11, 3, 9, 0, 0, 0, time.UTC)

func testAging() *Aging { return NewAging("heist", 1, storyStart, DefaultSpan) }

// The same game version must build the same image, or a rebuild silently
// changes every timestamp on the box.
func TestAgingIsDeterministic(t *testing.T) {
	a, b := testAging(), testAging()
	owner := Owner{User: "jposti"}

	for _, p := range []string{"/home/jposti/notes.txt", "/home/jposti/.bashrc", "/home/jposti/w/a.sh"} {
		if got, want := b.FileTime(owner, p), a.FileTime(owner, p); !got.Equal(want) {
			t.Errorf("%s: %v then %v", p, want, got)
		}
	}
}

// Timestamps are hashed from paths rather than drawn from a stream, so that an
// author adding one file does not re-age the whole game.
func TestAddingAFileDoesNotDisturbTheRest(t *testing.T) {
	a := testAging()
	owner := Owner{User: "jposti"}

	before := a.FileTime(owner, "/home/jposti/existing.txt")
	_ = a.FileTime(owner, "/home/jposti/brand-new.txt")
	after := a.FileTime(owner, "/home/jposti/existing.txt")

	if !before.Equal(after) {
		t.Fatalf("an unrelated file moved from %v to %v", before, after)
	}
}

// The tell this whole feature exists to remove: a filesystem written in one
// second.
func TestFilesAreNotAllStampedTogether(t *testing.T) {
	a := testAging()
	owner := Owner{User: "jposti"}

	seen := map[time.Time]string{}
	for i := 0; i < 200; i++ {
		p := fmt.Sprintf("/home/jposti/dir%d/file%d.txt", i%12, i)
		at := a.FileTime(owner, p)
		if prior, dup := seen[at]; dup {
			t.Fatalf("%s and %s share an mtime (%v)", prior, p, at)
		}
		seen[at] = p
	}
}

// A file dated in the future is a harder tell than a file dated at build time.
func TestNothingIsNewerThanTheFictionalPresent(t *testing.T) {
	a := testAging()

	for _, user := range []string{"jposti", "bkup", "oncall", "dsundqvist"} {
		owner := Owner{User: user}
		for i := 0; i < 100; i++ {
			p := fmt.Sprintf("/home/%s/f%d", user, i)
			if at := a.FileTime(owner, p); at.After(a.Now()) {
				t.Fatalf("%s is dated %v, after the present %v", p, at, a.Now())
			}
		}
		if at := a.AccountCreated(owner); at.After(a.Now()) {
			t.Fatalf("account %s created at %v, after the present", user, at)
		}
	}
}

// Dotfiles carry the account's birth date on a real system and are never
// touched again. When they instead match the rest of the tree, the account
// reads as fabricated.
func TestAccountFilesPredateTheAccountsWork(t *testing.T) {
	a := testAging()
	owner := Owner{User: "jposti"}

	created := a.AccountCreated(owner)
	if got := a.FileTime(owner, "/home/jposti/.bashrc"); !got.Equal(created) {
		t.Errorf(".bashrc = %v, want the account date %v", got, created)
	}
	if !created.Before(a.Start()) {
		t.Errorf("account created %v; accounts should predate the recorded timeline (%v)", created, a.Start())
	}

	work := a.FileTime(owner, "/home/jposti/notes.txt")
	if !created.Before(work) {
		t.Errorf("account date %v is not older than the account's work %v", created, work)
	}
}

// A project directory should read as a sitting of work, not a scatter across
// the whole timeline.
func TestFilesClusterByDirectory(t *testing.T) {
	a := testAging()
	owner := Owner{User: "bkup"}

	spread := func(paths []string) time.Duration {
		var min, max time.Time
		for i, p := range paths {
			at := a.FileTime(owner, p)
			if i == 0 || at.Before(min) {
				min = at
			}
			if i == 0 || at.After(max) {
				max = at
			}
		}
		return max.Sub(min)
	}

	// Stale outliers are deliberate, so compare directories chosen to have none.
	var sameDir, acrossDirs []string
	for i := 0; i < 8; i++ {
		sameDir = append(sameDir, fmt.Sprintf("/home/bkup/project/f%d.sh", i))
		acrossDirs = append(acrossDirs, fmt.Sprintf("/home/bkup/d%d/f.sh", i))
	}

	within, across := spread(sameDir), spread(acrossDirs)
	if within > across {
		t.Errorf("one directory spans %v but eight directories span %v; clustering is not happening", within, across)
	}
}

// Two accounts writing at exactly the same moments would look like one hand.
func TestUsersHaveDistinctActivePeriods(t *testing.T) {
	a := testAging()

	centre := func(user string) time.Time {
		var sum int64
		const n = 40
		for i := 0; i < n; i++ {
			sum += a.FileTime(Owner{User: user}, fmt.Sprintf("/home/%s/d%d/f", user, i)).Unix() / n
		}
		return time.Unix(sum, 0)
	}

	a1, a2 := centre("jposti"), centre("dsundqvist")
	if d := a1.Sub(a2); d < time.Hour && d > -time.Hour {
		t.Errorf("two users' activity centres are %v apart; they read as one person", d)
	}
}

// Logs run up to the present. A box whose newest log line is two months old is
// a box nobody is running.
func TestLogsAreRecent(t *testing.T) {
	a := testAging()
	const n = 50

	newest := a.LogTime("auth.log", n-1, n)
	if a.Now().Sub(newest) > a.Now().Sub(a.Start())/8 {
		t.Errorf("newest log line is %v, far behind the present %v", newest, a.Now())
	}
	if a.LogTime("auth.log", 0, n).After(newest) {
		t.Error("log lines are not ordered oldest to newest")
	}
}

// Shell history that reads backwards is worse than no history at all.
func TestSpreadIsMonotonic(t *testing.T) {
	a := testAging()
	times := a.Spread("jposti", 25)

	if len(times) != 25 {
		t.Fatalf("Spread returned %d times", len(times))
	}
	for i := 1; i < len(times); i++ {
		if times[i].Before(times[i-1]) {
			t.Fatalf("entry %d (%v) precedes entry %d (%v)", i, times[i], i-1, times[i-1])
		}
	}
	if times[len(times)-1].After(a.Now()) {
		t.Error("history runs past the present")
	}
}

// Some files must be much older than their neighbours, or every tree looks
// uniformly and suspiciously fresh.
func TestSomeFilesAreMuchOlder(t *testing.T) {
	a := testAging()
	owner := Owner{User: "oncall"}

	var stale int
	const n = 300
	cutoff := a.Start().Add(a.Now().Sub(a.Start()) / 4)
	for i := 0; i < n; i++ {
		if a.FileTime(owner, fmt.Sprintf("/home/oncall/d%d/f%d", i%20, i)).Before(cutoff) {
			stale++
		}
	}
	if stale == 0 {
		t.Fatal("no file is materially older than the rest; the tree reads as uniform")
	}
	if stale > n/2 {
		t.Fatalf("%d of %d files are stale; the box reads as abandoned", stale, n)
	}
}
