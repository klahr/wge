// Package build compiles a game directory into a container image, and seeds a
// container from that image with one run's credentials.
//
// The split matters. An image is shared by every run of a game version, so it
// can never contain a password: what it holds are placeholders. Rendering
// happens per run, against a container, immediately before the player is let
// in. See seed.go.
package build

import (
	"crypto/sha256"
	"encoding/binary"
	"path"
	"strings"
	"time"
)

// Aging assigns plausible timestamps to every file in a game.
//
// This is the single highest-leverage realism feature in the engine. A freshly
// built image stamps every file with the build time, and `ls -la` gives that
// away in about four seconds -- a whole filesystem written in the same second
// is not a machine anyone has worked on. Real boxes are stratified: dotfiles
// from the day the account was made, a project touched last spring, logs from
// this morning.
//
// Timestamps are derived by hashing paths rather than drawn from a random
// stream, so they are reproducible (the same game version always builds the
// same image) and stable under edits: adding one file does not shift the age of
// every other file.
type Aging struct {
	start time.Time
	now   time.Time
	seed  string
}

// DefaultSpan is how long a game's fictional history runs when the manifest
// does not say. Long enough to stratify, short enough that everything on the
// box still belongs to one era.
const DefaultSpan = 90 * 24 * time.Hour

// AnchorLabel is the image label carrying the resolved timeline anchor.
const AnchorLabel = "wge.timeline.start"

// ResolveAnchor returns the date a game's recorded history begins.
//
// A game that does not pin a date gets one relative to the build, so that the
// fictional present and the real present are the same moment. That matters as
// soon as anything on the box is alive: a cron job that fires, a service that
// appends to its log. Those write with the real clock, and against a fixed past
// date every one of them lands months after the newest file on a box that is
// otherwise carefully aged.
//
// The resolved value is recorded on the image, because everything downstream --
// seeding, verification -- has to age against the same anchor the build used.
func ResolveAnchor(start time.Time, span time.Duration, built time.Time) time.Time {
	if !start.IsZero() {
		return start
	}
	if span <= 0 {
		span = DefaultSpan
	}
	return built.Add(-span)
}

// NewAging returns the aging plan for a game version.
func NewAging(gameID string, version int, start time.Time, span time.Duration) *Aging {
	if span <= 0 {
		span = DefaultSpan
	}
	return &Aging{
		start: start,
		now:   start.Add(span),
		seed:  gameID + "\x1f" + itoa(version),
	}
}

// Now is the fictional present: the newest a file on this box may be.
func (a *Aging) Now() time.Time { return a.now }

// Start is the oldest timestamp the box carries.
func (a *Aging) Start() time.Time { return a.start }

// Provisioned is when the machine itself was installed.
//
// Everything the base image contributes is dated from the image build -- which
// is to say, today -- and a /usr full of files modified this afternoon says the
// box was made this afternoon. Stamping them to a single provisioning date is
// not a compromise: it is what a real install looks like, since package files
// land together and only the ones with older upstream dates keep them.
//
// It sits comfortably before any account, so nobody is created on a machine
// that did not yet exist.
func (a *Aging) Provisioned() time.Time {
	span := a.now.Sub(a.start)
	back := scale(a.fraction("provisioned", a.seed), 5*float64(span), 7*float64(span))
	return a.start.Add(-time.Duration(back))
}

// accountFiles are created when an account is created. On a real system they
// carry the account's birth date and are never touched again, which makes them
// one of the clearest tells when they don't.
var accountFiles = map[string]bool{
	".bashrc":          true,
	".bash_logout":     true,
	".profile":         true,
	".bash_profile":    true,
	".zshrc":           true,
	".selected_editor": true,
}

// Owner describes whose files are being aged. Each account has its own creation
// date and its own period of activity, so two users' home directories do not
// look like they were written by the same hand at the same moment.
type Owner struct {
	// User is the account name; it seeds the account's dates.
	User string
}

// FileTime returns the mtime for one path in an owner's tree.
//
// Files cluster three ways, because that is how they cluster in life:
//
//   - Account dotfiles land on the account's creation date.
//   - Everything else clusters around the account's period of activity...
//   - ...and, within that, around a per-directory moment, so a project
//     directory reads as one sitting of work rather than a scatter.
func (a *Aging) FileTime(owner Owner, filePath string) time.Time {
	if accountFiles[path.Base(filePath)] {
		return a.AccountCreated(owner)
	}

	span := a.now.Sub(a.start)

	// The account's active period: a window inside the timeline, not the whole
	// of it. Users arrive and drift away.
	activeCentre := a.start.Add(time.Duration(scale(a.fraction("active", owner.User), float64(span)/5, float64(span))))
	activeWidth := span / 6

	// Each directory is a sitting of work.
	dir := path.Dir(filePath)
	dirOffset := scale(a.fraction("dir", owner.User+"\x1f"+dir), -1, 1)
	dirCentre := activeCentre.Add(time.Duration(dirOffset * float64(activeWidth)))

	// And each file moves a little around its sitting.
	fileOffset := scale(a.fraction("file", owner.User+"\x1f"+filePath), -1, 1)
	at := dirCentre.Add(time.Duration(fileOffset * float64(activeWidth/4)))

	// A minority of files are much older than the rest -- things carried
	// forward from an old machine, or written once years ago and never opened
	// again. Without this every tree looks suspiciously uniform.
	if a.fraction("stale", owner.User+"\x1f"+filePath) < 0.15 {
		at = at.Add(-time.Duration(a.fraction("stale-by", filePath) * float64(span)))
	}

	return a.clamp(at)
}

// AccountCreated is when an account came into existence. Accounts are created
// before the period they are active in, and never after the box's present.
func (a *Aging) AccountCreated(owner Owner) time.Time {
	span := a.now.Sub(a.start)
	// Accounts predate the recorded timeline: a company's mail server did not
	// spring into being with its logs.
	back := scale(a.fraction("account", owner.User), float64(span), 4*float64(span))
	return a.start.Add(-time.Duration(back))
}

// LogTime returns a timestamp for a log line at position i of n, spread across
// the tail of the timeline. Logs are the freshest thing on a box: they run up
// to the present, which is what makes a box feel like it is still running.
func (a *Aging) LogTime(name string, i, n int) time.Time {
	if n <= 0 {
		return a.now
	}
	// Logs cover the last tenth of the timeline, oldest first.
	window := a.now.Sub(a.start) / 10
	base := a.now.Add(-window)

	progress := float64(i) / float64(n)
	jitter := scale(a.fraction("log", name+itoa(i)), -0.4, 0.4) * float64(window) / float64(n)

	return a.clamp(base.Add(time.Duration(progress*float64(window) + jitter)))
}

// SessionTime returns the start of a login session for wtmp, spread across the
// timeline so that last(1) shows a history rather than a burst.
func (a *Aging) SessionTime(user string, i, n int) time.Time {
	if n <= 0 {
		return a.now
	}
	span := a.now.Sub(a.start)
	// Sessions run oldest to newest with the most recent close to the present.
	progress := float64(i+1) / float64(n+1)
	jitter := scale(a.fraction("session", user+itoa(i)), -0.3, 0.3) * float64(span) / float64(n+1)

	return a.clamp(a.start.Add(time.Duration(progress*float64(span) + jitter)))
}

func (a *Aging) clamp(t time.Time) time.Time {
	// Nothing may be newer than the fictional present. A file dated tomorrow is
	// a harder tell than a file dated at build time.
	if t.After(a.now) {
		return a.now.Add(-time.Duration(a.fraction("clamp", t.String()) * float64(time.Hour)))
	}
	// And nothing older than the box itself, unless it is an account date,
	// which is computed before the start deliberately.
	oldest := a.start.Add(-4 * a.now.Sub(a.start))
	if t.Before(oldest) {
		return oldest
	}
	return t
}

// fraction hashes a key into a uniform value in [0, 1). Hashing rather than
// drawing from a stream keeps the plan stable: editing one file does not
// re-age the rest of the game.
func (a *Aging) fraction(namespace, key string) float64 {
	sum := sha256.Sum256([]byte(a.seed + "\x1f" + namespace + "\x1f" + key))
	// 53 bits is the mantissa of a float64; taking more would not add precision.
	n := binary.BigEndian.Uint64(sum[:8]) >> 11
	return float64(n) / float64(uint64(1)<<53)
}

// scale maps a [0,1) fraction onto an arbitrary range.
func scale(f, lo, hi float64) float64 {
	return lo + f*(hi-lo)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

// Spread returns n timestamps across the timeline, sorted oldest first. It is
// used where a sequence needs plausible spacing rather than individual
// placement -- shell history, mail, log files.
func (a *Aging) Spread(name string, n int) []time.Time {
	out := make([]time.Time, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, a.SessionTime(name, i, n))
	}
	// SessionTime is monotonic in i up to jitter; a light pass restores order
	// so that a history never reads backwards.
	for i := 1; i < len(out); i++ {
		if out[i].Before(out[i-1]) {
			out[i] = out[i-1].Add(time.Duration(a.fraction("nudge", name+itoa(i)) * float64(time.Hour)))
		}
	}
	return out
}

// IsAccountFile reports whether a path is one of the files created with an
// account, exposed for the tests that guard the stratification.
func IsAccountFile(p string) bool { return accountFiles[path.Base(strings.TrimSuffix(p, "/"))] }
