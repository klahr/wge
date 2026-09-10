package build

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/klahr/wge/internal/creds"
	"github.com/klahr/wge/internal/docker"
	"github.com/klahr/wge/internal/manifest"

	"golang.org/x/crypto/ssh"
)

// Seeder installs one run's credentials into a container built from a shared
// image.
//
// An image is shared by every run of a game version, so it cannot contain a
// password: what it holds are unrendered templates. Seeding renders them
// against the run's derived secrets and sets the accounts' passwords, in the
// window between the container starting and the player being let in.
//
// Nothing is written to disk on the way. Rendered files are uploaded as a tar,
// and passwords are piped to chpasswd, so the container never holds the payload
// anywhere a player could find it -- and the image needs no tooling of its own,
// which is just as well, since a /usr/local/bin/wge-seed would be the loudest
// thing on the box.
//
// A seeder holds no engine of its own. It is handed the one the container is
// on, which in a pool is not always the local machine -- and a seeder that
// assumed otherwise would render a run's credentials into thin air and leave
// the player looking at template text.
type Seeder struct{}

// NewSeeder returns a seeder.
func NewSeeder() *Seeder { return &Seeder{} }

// RunSecrets is what a template can reach. It is the run's derived credentials
// dressed up as template functions.
type RunSecrets struct {
	game    *manifest.Game
	deriver *creds.Deriver
	handle  string
}

// NewRunSecrets returns the template context for one run.
func NewRunSecrets(g *manifest.Game, d *creds.Deriver, handle string) *RunSecrets {
	return &RunSecrets{game: g, deriver: d, handle: handle}
}

// Handle is the player's name, for correspondence addressed to them.
func (r *RunSecrets) Handle() string { return r.handle }

// Password returns a level's login password.
func (r *RunSecrets) Password(levelID string) (string, error) {
	if _, ok := r.game.Level(levelID); !ok {
		return "", fmt.Errorf("no level %q", levelID)
	}
	return r.deriver.Password(levelID), nil
}

// Passphrase returns the passphrase protecting a level's private key.
func (r *RunSecrets) Passphrase(levelID string) (string, error) {
	if _, ok := r.game.Level(levelID); !ok {
		return "", fmt.Errorf("no level %q", levelID)
	}
	return r.deriver.Passphrase(levelID), nil
}

// User returns a level's account name, so narrative text can name an account
// without the author having to keep two files in step.
func (r *RunSecrets) User(levelID string) (string, error) {
	l, ok := r.game.Level(levelID)
	if !ok {
		return "", fmt.Errorf("no level %q", levelID)
	}
	return l.User, nil
}

// PrivateKey returns a level's OpenSSH private key, encrypted with that level's
// passphrase. Splitting a key from its passphrase across two levels is what
// gives the graph a genuine second parent.
func (r *RunSecrets) PrivateKey(levelID string) (string, error) {
	if _, ok := r.game.Level(levelID); !ok {
		return "", fmt.Errorf("no level %q", levelID)
	}

	key := r.deriver.SSHKey(levelID)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(key, levelID, []byte(r.deriver.Passphrase(levelID)))
	if err != nil {
		return "", fmt.Errorf("marshal key for %s: %w", levelID, err)
	}
	return string(pem.EncodeToMemory(block)), nil
}

// UnencryptedPrivateKey returns a level's key with no passphrase, for the games
// that gate on possession of the key alone.
func (r *RunSecrets) UnencryptedPrivateKey(levelID string) (string, error) {
	if _, ok := r.game.Level(levelID); !ok {
		return "", fmt.Errorf("no level %q", levelID)
	}

	block, err := ssh.MarshalPrivateKey(r.deriver.SSHKey(levelID), levelID)
	if err != nil {
		return "", fmt.Errorf("marshal key for %s: %w", levelID, err)
	}
	return string(pem.EncodeToMemory(block)), nil
}

// HostKey returns a host's public key as a known_hosts entry, so a game can
// ship a known_hosts that is actually correct.
func (r *RunSecrets) HostKey(host string) (string, error) {
	for _, id := range r.game.HostIDs() {
		if id == host {
			return HostKeyLine(r.game.ID, r.game.Version, host)
		}
	}
	return "", fmt.Errorf("no host %q", host)
}

// PublicKey returns a level's public key as an authorized_keys line.
func (r *RunSecrets) PublicKey(levelID string) (string, error) {
	l, ok := r.game.Level(levelID)
	if !ok {
		return "", fmt.Errorf("no level %q", levelID)
	}

	pub, err := ssh.NewPublicKey(r.deriver.SSHKey(levelID).Public())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + l.User, nil
}

// templateMarker is what makes a file a template. Anything containing it is
// rendered at seed time; everything else is installed as-is at build time.
const templateMarker = "{{"

// Seed renders a run's templates into its container and sets the account
// passwords. It must complete before a player is attached: an unseeded
// container has placeholder text where its credentials should be.
func (s *Seeder) Seed(ctx context.Context, api *docker.Client, container string, g *manifest.Game, host string, secrets *RunSecrets) error {
	anchor, err := AnchorOf(ctx, api, container)
	if err != nil {
		return err
	}

	rendered, err := RenderTemplates(g, host, anchor, secrets)
	if err != nil {
		return err
	}

	if len(rendered) > 0 {
		// The image's permissions win. A tar header always carries a mode, so
		// uploading the author's on-disk mode would silently undo whatever the
		// level's setup.sh established -- and setup.sh is where a game's access
		// control is expressed. Seeding replaces content, nothing else.
		if err := s.adoptExistingMetadata(ctx, api, container, rendered); err != nil {
			return err
		}

		archive, err := tarEntries(rendered)
		if err != nil {
			return fmt.Errorf("pack rendered files: %w", err)
		}
		if err := api.PutArchive(ctx, container, "/", bytes.NewReader(archive)); err != nil {
			return fmt.Errorf("upload rendered files: %w", err)
		}
	}

	if err := s.setPasswords(ctx, api, container, g, host, secrets); err != nil {
		return err
	}
	return s.restoreTimes(ctx, api, container, g, host, anchor, rendered)
}

// setPasswords writes the level accounts' passwords via chpasswd on stdin.
//
// The passwords never touch the container's filesystem except as hashes in
// /etc/shadow, which is where a player would expect to find them and where they
// are of no use: the hash is yescrypt over 24 characters of derived entropy.
func (s *Seeder) setPasswords(ctx context.Context, api *docker.Client, container string, g *manifest.Game, host string, secrets *RunSecrets) error {
	var lines strings.Builder
	for _, l := range g.Levels {
		if l.Host != host {
			continue
		}
		password, err := secrets.Password(l.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(&lines, "%s:%s\n", l.User, password)
	}
	if lines.Len() == 0 {
		return nil
	}

	res, err := api.Exec(ctx, container, docker.ExecOptions{
		Cmd:   []string{"chpasswd"},
		User:  "root",
		Stdin: strings.NewReader(lines.String()),
	})
	if err != nil {
		return fmt.Errorf("set level passwords: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("chpasswd exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// restoreTimes puts back every timestamp seeding disturbed.
//
// Seeding happens at container start, which is the real present and not the
// fictional one, and it disturbs more than the files it writes. chpasswd
// rewrites /etc/shadow; uploading a rendered file re-dates the directory that
// holds it. Left alone, a home directory whose contents are months old sits
// inside a folder modified sixty seconds ago -- and that timestamp is the
// moment this player's container was created, which is a fact about the game
// engine rather than about the box.
func (s *Seeder) restoreTimes(
	ctx context.Context, api *docker.Client, container string, g *manifest.Game, host string,
	anchor time.Time, rendered []entry,
) error {
	plan, err := Assemble(g, host, anchor)
	if err != nil {
		return err
	}
	aging := NewAging(g.ID, g.Version, anchor, g.Timeline.Span.Duration())
	owner := Owner{User: "root"}

	// Ordered, so a parent is stamped after the child whose write disturbed it.
	var targets []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			targets = append(targets, p)
		}
	}

	var disturbed []string
	for _, e := range rendered {
		disturbed = append(disturbed, e.Path)
	}
	disturbed = append(disturbed, accountDatabases...)
	disturbed = append(disturbed, runtimeFiles...)

	for _, target := range disturbed {
		add(target)
		for dir := path.Dir(target); dir != "/" && dir != "."; dir = path.Dir(dir) {
			add(dir)
		}
	}

	// Deepest first: touching a directory does not disturb its parent, but
	// stamping in this order keeps the intent obvious.
	sort.Slice(targets, func(i, j int) bool {
		return strings.Count(targets[i], "/") > strings.Count(targets[j], "/")
	})

	var script strings.Builder

	// Sweep the directories the build could not date.
	//
	// Docker's layer diff drops a directory whose only change is its mtime, so
	// the build-time sweep silently loses every empty directory it stamps --
	// /opt, /mnt, /etc/apt/keyrings and a couple of hundred others keep the
	// base image's build date. The game's own directories survive because
	// their contents changed in the same layer; these have nothing in them to
	// change. A live container has no layer diff, so doing it here works.
	fmt.Fprintf(&script, `planned=$(mktemp)
newer=$(mktemp)
sort -u > "$planned"
find / -xdev -type d -newermt @%d 	-not -path '/proc/*' -not -path '/sys/*' -not -path '/dev/*' 2>/dev/null 	| sort -u > "$newer"
comm -23 "$newer" "$planned" | xargs -d '
' -r touch -d @%d -- 2>/dev/null
rm -f "$planned" "$newer"
`, aging.Provisioned().Unix(), aging.Provisioned().Unix())

	// Then the individual dates, which must win over the sweep.
	for _, target := range targets {
		at, ok := plan.ModTime(target)
		if !ok {
			// Not something the game placed -- a system directory, or a file
			// the build created. It still needs a plausible date.
			at = aging.FileTime(owner, target)
		}
		fmt.Fprintf(&script, "[ -e %q ] && touch -h -d @%d -- %q\n", target, at.Unix(), target)
	}
	script.WriteString("exit 0\n")

	// The sweep's exclusion list is every path the plan names, so a game's own
	// directories keep the dates the build gave them.
	var planned strings.Builder
	for _, line := range strings.Split(string(plan.rootfs.metaPlan()), "\n") {
		if fields := strings.Split(line, "\t"); len(fields) == 3 {
			planned.WriteString(fields[2])
			planned.WriteString("\n")
		}
	}

	res, err := api.Exec(ctx, container, docker.ExecOptions{
		Cmd:   []string{"sh", "-c", script.String()},
		User:  "root",
		Stdin: strings.NewReader(planned.String()),
	})
	if err != nil {
		return fmt.Errorf("restore timestamps: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("restoring timestamps exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// adoptExistingMetadata replaces each rendered file's mode and ownership with
// whatever the built image already gives that path.
//
// The build placed an unrendered copy of every template, so the path exists and
// carries the permissions the image intends. Anything the seeder cannot stat is
// left with the values from the plan, which is the right fallback for a file the
// build did not manage to place.
func (s *Seeder) adoptExistingMetadata(ctx context.Context, api *docker.Client, container string, entries []entry) error {
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}

	res, err := api.Exec(ctx, container, docker.ExecOptions{
		Cmd: []string{"sh", "-c",
			`while IFS= read -r p; do stat -c '%a %u %g %n' -- "$p" 2>/dev/null || true; done`},
		User:  "root",
		Stdin: strings.NewReader(strings.Join(paths, "\n") + "\n"),
	})
	if err != nil {
		return fmt.Errorf("read existing file metadata: %w", err)
	}

	type meta struct {
		mode     fs.FileMode
		uid, gid int
	}
	existing := map[string]meta{}

	for _, line := range strings.Split(res.Stdout, "\n") {
		fields := strings.SplitN(strings.TrimRight(line, "\r"), " ", 4)
		if len(fields) != 4 {
			continue
		}
		mode, err := strconv.ParseUint(fields[0], 8, 32)
		if err != nil {
			continue
		}
		uid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		gid, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		existing[fields[3]] = meta{mode: fs.FileMode(mode), uid: uid, gid: gid}
	}

	for i := range entries {
		if m, ok := existing[entries[i].Path]; ok {
			entries[i].Mode = m.mode
			entries[i].UID, entries[i].GID = m.uid, m.gid
		}
	}
	return nil
}

// RenderTemplates finds every templated file in a game and renders it against a
// run's secrets, returning entries ready to be uploaded.
//
// The aging plan is recomputed here rather than carried in the image. It is a
// pure function of the game id, version and path, so the seeder can restore the
// exact mtime a rendered file is supposed to have -- without which every file
// holding a credential would carry the container's start time and stand out
// from everything around it.
func RenderTemplates(g *manifest.Game, host string, anchor time.Time, secrets *RunSecrets) ([]entry, error) {
	accounts, err := planAccounts(g, host)
	if err != nil {
		return nil, err
	}
	aging := NewAging(g.ID, g.Version, anchor, g.Timeline.Span.Duration())

	byUser := map[string]*account{}
	for _, a := range accounts {
		byUser[a.User] = a
	}

	var out []entry
	for _, l := range g.Levels {
		if l.Host != host {
			continue
		}
		acct, ok := byUser[l.User]
		if !ok {
			continue
		}

		for _, tree := range []struct{ dir, base string }{
			{manifest.HomeDir, acct.Home},
			{manifest.FilesDir, "/"},
		} {
			rendered, err := renderTree(l, acct, tree.dir, tree.base, aging, secrets)
			if err != nil {
				return nil, err
			}
			out = append(out, rendered...)
		}

		mailEntry, ok, err := renderLevelMail(l, acct, aging, secrets)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, mailEntry)
		}
	}
	return out, nil
}

func renderTree(l *manifest.Level, acct *account, tree, base string, aging *Aging, secrets *RunSecrets) ([]entry, error) {
	root := filepath.Join(l.Dir, tree)
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}

	owner := Owner{User: acct.User}
	var out []entry

	err := filepath.WalkDir(root, func(src string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(root, src)
		if relErr != nil {
			return relErr
		}

		if d.IsDir() {
			if !isArchiveDir(d.Name()) {
				return nil
			}

			// An archive cannot have one member replaced in place, so seeding
			// builds the whole file again with its credentials rendered.
			dst := path.Join(base, filepath.ToSlash(rel))
			templated, err := archiveContainsTemplate(src)
			if err != nil || !templated {
				return err
			}

			content, err := packArchive(src, dst, aging, owner, acct.UID, acct.GID,
				func(member string, raw []byte) ([]byte, error) {
					if !bytes.Contains(raw, []byte(templateMarker)) {
						return raw, nil
					}
					return render(member, string(raw), secrets)
				})
			if err != nil {
				return err
			}

			info, err := d.Info()
			if err != nil {
				return err
			}
			out = append(out, entry{
				Path: dst, Content: content, Mode: info.Mode().Perm(),
				UID: acct.UID, GID: acct.GID,
				ModTime: aging.FileTime(owner, dst),
			})
			return fs.SkipDir
		}

		raw, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if !bytes.Contains(raw, []byte(templateMarker)) {
			return nil
		}

		dst := path.Join(base, filepath.ToSlash(rel))

		content, err := render(dst, string(raw), secrets)
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, entry{
			Path: dst, Content: content, Mode: info.Mode().Perm(),
			UID: acct.UID, GID: acct.GID,
			ModTime: aging.FileTime(owner, dst),
		})
		return nil
	})
	return out, err
}

func renderLevelMail(l *manifest.Level, acct *account, aging *Aging, secrets *RunSecrets) (entry, bool, error) {
	if len(l.Narrative.Mail) == 0 {
		return entry{}, false, nil
	}

	var messages []string
	var templated bool
	for _, rel := range l.Narrative.Mail {
		raw, err := os.ReadFile(filepath.Join(l.Dir, rel))
		if err != nil {
			return entry{}, false, fmt.Errorf("level %s: read mail %s: %w", l.ID, rel, err)
		}
		if bytes.Contains(raw, []byte(templateMarker)) {
			templated = true
		}
		rendered, err := render(rel, string(raw), secrets)
		if err != nil {
			return entry{}, false, err
		}
		messages = append(messages, string(rendered))
	}
	if !templated {
		return entry{}, false, nil
	}

	return entry{
		Path: path.Join("/var/mail", acct.User), Content: mailbox(messages),
		Mode: 0o660, UID: acct.UID, GID: acct.GID,
		ModTime: aging.LogTime("mail/"+acct.User, 9, 10),
	}, true, nil
}

func render(name, text string, secrets *RunSecrets) ([]byte, error) {
	// Missing keys must be an error rather than "<no value>": a level whose
	// credential silently renders as empty is a level nobody can solve.
	tmpl, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("parse template %s: %w", name, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, secrets); err != nil {
		return nil, fmt.Errorf("render %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// tarEntries packs rendered files for upload, carrying ownership, mode and the
// aged mtime in the headers so the container needs to do nothing but extract.
func tarEntries(entries []entry) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: strings.TrimPrefix(e.Path, "/"), Mode: int64(e.Mode.Perm()),
			Uid: e.UID, Gid: e.GID, Size: int64(len(e.Content)),
			ModTime: e.ModTime, Typeflag: tar.TypeReg, Format: tar.FormatPAX,
		}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(e.Content); err != nil {
			return nil, err
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// AnchorOf reads the timeline anchor the build recorded on a container's image.
//
// It travels as a label rather than a file because a label is invisible from
// inside the container. Everything downstream of the build has to age against
// the clock the build used, and for a game with a relative timeline that clock
// is not derivable from the manifest alone.
func AnchorOf(ctx context.Context, api *docker.Client, container string) (time.Time, error) {
	var inspect struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := api.Get(ctx, "/containers/"+container+"/json", &inspect); err != nil {
		return time.Time{}, fmt.Errorf("read timeline anchor: %w", err)
	}

	raw, ok := inspect.Config.Labels[AnchorLabel]
	if !ok {
		return time.Time{}, fmt.Errorf("image carries no %s label; rebuild the game", AnchorLabel)
	}
	unix, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("malformed %s label %q: %w", AnchorLabel, raw, err)
	}
	return time.Unix(unix, 0).UTC(), nil
}

// runtimeFiles are written by the container runtime when the container starts,
// so the image cannot carry a date for them. Left alone they are the only
// things on the box stamped with the real present, which makes them the one
// place a player can read the engine's clock rather than the game's.
var runtimeFiles = []string{
	"/etc/hostname", "/etc/hosts", "/etc/resolv.conf", "/etc/mtab",
}

// SeedTimeout bounds seeding. A container that cannot be seeded must not be
// handed to a player: they would find placeholders where the game should be.
const SeedTimeout = 30 * time.Second
