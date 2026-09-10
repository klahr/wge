package build

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/klahr/wge/internal/creds"
	"github.com/klahr/wge/internal/docker"
	"github.com/klahr/wge/internal/manifest"
)

// Verifier proves a built image's permissions from inside a running container.
//
// The static validator checks that a game's graph and its credential
// placements agree. This checks the thing the manifest cannot express: whether
// the filesystem the build actually produced enforces that graph. Those are
// different questions, and the gap between them is where authored games break
// -- a setup script that forgets a chmod, a directory left traversable, a
// credential that ends up in a log.
//
// Nothing here reasons about permissions. Every check is performed by running
// as the level's own uid and attempting the read.
type Verifier struct {
	api    *docker.Client
	seeder *Seeder
}

// NewVerifier returns a verifier talking to the engine at socket.
func NewVerifier(socket string) *Verifier {
	return &Verifier{api: docker.New(socket), seeder: NewSeeder()}
}

// FindingKind classifies what went wrong.
type FindingKind string

const (
	// FindingUnsolvable is a credential a level cannot reach, which makes the
	// level after it unreachable.
	FindingUnsolvable FindingKind = "unsolvable"
	// FindingLeak is a file a level can read that belongs to a level it has
	// not earned its way to.
	FindingLeak FindingKind = "leak"
	// FindingCredentialLeak is a credential appearing in a file readable by a
	// level that should not have it. This is the one that ends a game: it
	// short-circuits the graph regardless of what the graph says.
	FindingCredentialLeak FindingKind = "credential-leak"
	// FindingSetuid is a setuid binary the base image did not have.
	FindingSetuid FindingKind = "setuid"
)

// Finding is one problem with a built image.
type Finding struct {
	Kind   FindingKind
	Level  string
	Path   string
	Detail string
}

func (f Finding) String() string {
	if f.Level == "" {
		return fmt.Sprintf("%s: %s: %s", f.Kind, f.Path, f.Detail)
	}
	return fmt.Sprintf("%s: %s: %s: %s", f.Kind, f.Level, f.Path, f.Detail)
}

// Report is the outcome of verifying an image.
type Report struct {
	Image    string
	Levels   int
	Checks   int
	Findings []Finding
}

// OK reports whether the image is fit to serve.
func (r *Report) OK() bool { return len(r.Findings) == 0 }

// Verify boots the image, seeds it with a throwaway run, and checks it.
//
// A throwaway salt is used rather than any player's: the credentials have to
// exist for the checks to mean anything, and they must not be a run anyone is
// playing.
func (v *Verifier) Verify(ctx context.Context, g *manifest.Game, host, image string) (*Report, error) {
	salt, err := creds.NewSalt()
	if err != nil {
		return nil, err
	}
	secrets := NewRunSecrets(g, creds.New(salt, g.ID), "verify")

	container := fmt.Sprintf("wge-verify-%s-%s-%s", g.ID, host, salt.String()[:8])
	if err := v.start(ctx, container, image, host); err != nil {
		return nil, err
	}
	defer func() {
		q := "?force=true&v=true"
		_ = v.api.Delete(context.WithoutCancel(ctx), "/containers/"+container+q)
	}()

	if err := v.seeder.Seed(ctx, v.api, container, g, host, secrets); err != nil {
		return nil, fmt.Errorf("seed verification container: %w", err)
	}

	anchor, err := AnchorOf(ctx, v.api, container)
	if err != nil {
		return nil, err
	}
	plan, err := Assemble(g, host, anchor)
	if err != nil {
		return nil, err
	}

	report := &Report{Image: image}
	levels := levelsOnHost(g, host)
	report.Levels = len(levels)

	for _, l := range levels {
		findings, checks, err := v.verifyLevel(ctx, container, g, plan, l, secrets)
		if err != nil {
			return nil, err
		}
		report.Findings = append(report.Findings, findings...)
		report.Checks += checks
	}

	archives, checks, err := v.verifyArchives(ctx, container, g, levels)
	if err != nil {
		return nil, err
	}
	report.Findings = append(report.Findings, archives...)
	report.Checks += checks

	setuid, err := v.verifySetuid(ctx, container, plan.base(), host)
	if err != nil {
		return nil, err
	}
	report.Findings = append(report.Findings, setuid...)
	report.Checks++

	return report, nil
}

func (v *Verifier) start(ctx context.Context, container, image, host string) error {
	body := map[string]any{
		"Image":    image,
		"Hostname": host,
		"Labels":   map[string]string{"wge.verify": "1"},
		"HostConfig": map[string]any{
			"CapDrop": []string{"ALL"},
			"CapAdd": []string{
				"SETUID", "SETGID", "CHOWN", "FOWNER", "FSETID",
				"DAC_OVERRIDE", "KILL", "AUDIT_WRITE", "NET_BIND_SERVICE", "SYS_CHROOT",
			},
			"NetworkMode": "none",
			"AutoRemove":  false,
		},
	}
	if err := v.api.Post(ctx, "/containers/create?name="+container, body, nil); err != nil {
		return fmt.Errorf("create verification container: %w", err)
	}
	if err := v.api.Post(ctx, "/containers/"+container+"/start", nil, nil); err != nil {
		return fmt.Errorf("start verification container: %w", err)
	}
	return nil
}

func levelsOnHost(g *manifest.Game, host string) []*manifest.Level {
	var out []*manifest.Level
	for _, l := range g.Levels {
		if l.Host == host {
			out = append(out, l)
		}
	}
	return out
}

// reachable returns the levels a player standing on l has necessarily already
// solved, plus l itself. Anything outside that set is something l has not
// earned.
func reachable(g *manifest.Game, id string) map[string]bool {
	seen := map[string]bool{id: true}
	queue := []string{id}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		l, ok := g.Level(current)
		if !ok {
			continue
		}
		for _, req := range l.Requires {
			if !seen[req] {
				seen[req] = true
				queue = append(queue, req)
			}
		}
	}
	return seen
}

// mayKnow returns the levels whose credentials a player on l may legitimately
// hold: the ones behind them, and the ones this level exists to hand over.
func mayKnow(g *manifest.Game, l *manifest.Level) map[string]bool {
	known := reachable(g, l.ID)
	for _, gr := range l.Grants {
		known[gr.To] = true
	}
	return known
}

// verifyLevel runs every check for one level, as that level's own user.
func (v *Verifier) verifyLevel(
	ctx context.Context, container string, g *manifest.Game,
	plan *Plan, l *manifest.Level, secrets *RunSecrets,
) ([]Finding, int, error) {
	earned := reachable(g, l.ID)
	owned := plan.PathsByLevel()

	// Everything belonging to a level this one has not earned its way to.
	var forbidden []string
	for level, paths := range owned {
		if earned[level] {
			continue
		}
		forbidden = append(forbidden, paths...)
	}
	sort.Strings(forbidden)

	// The credentials this level holds must actually be where the manifest
	// says they are, or the level after it is unreachable.
	var required []string
	for _, gr := range l.Grants {
		required = append(required, gr.Path())
	}

	probes, err := v.probe(ctx, container, l.User, required, forbidden)
	if err != nil {
		return nil, 0, err
	}

	checked := len(required) + len(forbidden) + 1
	var findings []Finding
	for _, path := range required {
		if !probes[path] {
			findings = append(findings, Finding{
				Kind: FindingUnsolvable, Level: l.ID, Path: path,
				Detail: fmt.Sprintf("%s cannot read the credential it is supposed to find here", l.User),
			})
		}
	}
	for _, path := range forbidden {
		if probes[path] {
			findings = append(findings, Finding{
				Kind: FindingLeak, Level: l.ID, Path: path,
				Detail: fmt.Sprintf("%s can read this, and it belongs to a level %s has not reached", l.User, l.ID),
			})
		}
	}

	leaks, err := v.sweepCredentials(ctx, container, g, l, secrets)
	if err != nil {
		return nil, 0, err
	}
	findings = append(findings, leaks...)

	// A service account is a node in the permission graph too. Whoever
	// compromises the daemon holds its uid, so anything that uid can read is
	// effectively readable from the level the service is reachable from -- and
	// a service quietly given more reach than its level collapses the graph
	// without the manifest saying anything about it.
	for _, svc := range l.Services {
		probes, err := v.probe(ctx, container, svc.User, forbidden)
		if err != nil {
			return nil, 0, err
		}
		for _, path := range forbidden {
			if probes[path] {
				findings = append(findings, Finding{
					Kind: FindingLeak, Level: l.ID, Path: path,
					Detail: fmt.Sprintf("service %q runs as %s, which can read this; compromising the daemon reaches a level %s has not",
						svc.Name, svc.User, l.ID),
				})
			}
		}
		checked += len(forbidden)
	}

	return findings, checked, nil
}

// probe asks, as the given user, which of these paths can be read.
//
// Readability is tested by attempting it as that uid rather than by reasoning
// about modes and groups. A permission model derived from the manifest would
// only ever agree with itself.
func (v *Verifier) probe(ctx context.Context, container, user string, groups ...[]string) (map[string]bool, error) {
	var all []string
	for _, g := range groups {
		all = append(all, g...)
	}
	if len(all) == 0 {
		return map[string]bool{}, nil
	}

	script := `while IFS= read -r p; do
	if [ -r "$p" ]; then printf 'R\t%s\n' "$p"; else printf 'N\t%s\n' "$p"; fi
done`

	res, err := v.api.Exec(ctx, container, docker.ExecOptions{
		Cmd:   []string{"sh", "-c", script},
		User:  user,
		Stdin: strings.NewReader(strings.Join(all, "\n") + "\n"),
	})
	if err != nil {
		return nil, fmt.Errorf("probe as %s: %w", user, err)
	}

	readable := map[string]bool{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		verdict, path, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		readable[path] = verdict == "R"
	}
	return readable, nil
}

// sweepCredentials looks for any credential this level should not hold, in any
// file this level can read.
//
// This is the check that matters most. The path-ownership checks assume a
// credential stays where the author put it; this one makes no such assumption,
// and catches a password copied into a log, left in a backup, or sitting in a
// world-readable config that nothing in the manifest mentions.
func (v *Verifier) sweepCredentials(
	ctx context.Context, container string, g *manifest.Game,
	l *manifest.Level, secrets *RunSecrets,
) ([]Finding, error) {
	known := mayKnow(g, l)

	// secret text -> the level it belongs to
	owners := map[string]string{}
	var args []string
	for _, other := range g.Levels {
		if known[other.ID] {
			continue
		}
		password, err := secrets.Password(other.ID)
		if err != nil {
			return nil, err
		}
		passphrase, err := secrets.Passphrase(other.ID)
		if err != nil {
			return nil, err
		}
		for _, secret := range []string{password, passphrase} {
			owners[secret] = other.ID
			args = append(args, "-e", secret)
		}
	}
	if len(args) == 0 {
		return nil, nil
	}

	// One pass over everything this uid can read. -o prints the matched text,
	// which is what maps a hit back to the level whose credential it is.
	script := `set -u
list=$(mktemp)
find / -xdev -type f -readable \
	-not -path '/proc/*' -not -path '/sys/*' -not -path '/dev/*' \
	2>/dev/null > "$list"
xargs -a "$list" -d '\n' -r grep -o -H -F "$@" 2>/dev/null | head -200
rm -f "$list"
exit 0`

	res, err := v.api.Exec(ctx, container, docker.ExecOptions{
		Cmd:  append([]string{"sh", "-c", script, "sh"}, args...),
		User: l.User,
	})
	if err != nil {
		return nil, fmt.Errorf("credential sweep as %s: %w", l.User, err)
	}

	seen := map[string]bool{}
	var findings []Finding

	for _, line := range strings.Split(res.Stdout, "\n") {
		if line == "" {
			continue
		}
		// grep -H -o prints "path:match". A path may contain colons, but the
		// match is a known secret, so the split point is unambiguous.
		for secret, owner := range owners {
			if !strings.HasSuffix(line, ":"+secret) {
				continue
			}
			path := strings.TrimSuffix(line, ":"+secret)
			key := l.ID + "\x1f" + owner + "\x1f" + path
			if seen[key] {
				break
			}
			seen[key] = true

			findings = append(findings, Finding{
				Kind: FindingCredentialLeak, Level: l.ID, Path: path,
				Detail: fmt.Sprintf("%s can read %s's credential here, short-circuiting the level graph", l.User, owner),
			})
			break
		}
	}

	sort.Slice(findings, func(i, j int) bool { return findings[i].Path < findings[j].Path })
	return findings, nil
}

// verifyArchives checks who can reach a credential packed inside an archive.
//
// The credential sweep cannot see into one: it greps files, and a gzip is not
// text. So this check does not look at the contents at all -- the manifest
// already says which credential is inside which archive, and the only question
// left is who can open the archive. A level that can read it holds everything
// in it, whatever the file it is nested in claims about its own permissions.
func (v *Verifier) verifyArchives(
	ctx context.Context, container string, g *manifest.Game, levels []*manifest.Level,
) ([]Finding, int, error) {
	type packed struct {
		archive string
		target  string
		owner   string
	}

	var contents []packed
	for _, l := range levels {
		for _, gr := range l.Grants {
			if gr.Member() != "" {
				contents = append(contents, packed{archive: gr.Path(), target: gr.To, owner: l.ID})
			}
		}
	}
	if len(contents) == 0 {
		return nil, 0, nil
	}

	var findings []Finding
	var checks int

	for _, l := range levels {
		known := mayKnow(g, l)

		var archives []string
		for _, c := range contents {
			if !known[c.target] {
				archives = append(archives, c.archive)
			}
		}
		if len(archives) == 0 {
			continue
		}

		probes, err := v.probe(ctx, container, l.User, archives)
		if err != nil {
			return nil, 0, err
		}
		checks += len(archives)

		for _, c := range contents {
			if known[c.target] || !probes[c.archive] {
				continue
			}
			findings = append(findings, Finding{
				Kind: FindingCredentialLeak, Level: l.ID, Path: c.archive,
				Detail: fmt.Sprintf(
					"%s can open this archive, and %s's credential for %s is inside it",
					l.User, c.owner, c.target),
			})
		}
	}
	return findings, checks, nil
}

// verifySetuid reports setuid binaries the base image did not have.
//
// `find / -perm -4000` is the first thing a competent player runs, and it
// should return exactly what the author intended. Diffing against the base
// rather than an allowlist means the check does not go stale when the base
// image changes.
func (v *Verifier) verifySetuid(ctx context.Context, container, base, host string) ([]Finding, error) {
	const find = `find / -xdev -perm -4000 -type f 2>/dev/null | sort`

	game, err := v.api.Exec(ctx, container, docker.ExecOptions{
		Cmd: []string{"sh", "-c", find}, User: "root",
	})
	if err != nil {
		return nil, fmt.Errorf("audit setuid binaries: %w", err)
	}

	baseline, err := v.setuidBaseline(ctx, base, find)
	if err != nil {
		return nil, err
	}

	var findings []Finding
	for _, path := range strings.Split(strings.TrimSpace(game.Stdout), "\n") {
		if path == "" || baseline[path] {
			continue
		}
		findings = append(findings, Finding{
			Kind: FindingSetuid, Path: path,
			Detail: "setuid binary the base image does not have; anyone who runs it runs as its owner",
		})
	}
	return findings, nil
}

// setuidBaseline runs the same audit against the untouched base image.
func (v *Verifier) setuidBaseline(ctx context.Context, base, find string) (map[string]bool, error) {
	container := "wge-verify-base-" + strings.NewReplacer("/", "-", ":", "-").Replace(base)

	if err := v.start(ctx, container, base, "base"); err != nil {
		return nil, err
	}
	defer func() {
		_ = v.api.Delete(context.WithoutCancel(ctx), "/containers/"+container+"?force=true&v=true")
	}()

	res, err := v.api.Exec(ctx, container, docker.ExecOptions{
		Cmd: []string{"sh", "-c", find}, User: "root",
	})
	if err != nil {
		return nil, fmt.Errorf("audit base image: %w", err)
	}

	baseline := map[string]bool{}
	for _, path := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if path != "" {
			baseline[path] = true
		}
	}
	return baseline, nil
}
