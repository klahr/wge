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
type Seeder struct {
	api *docker.Client
}

// NewSeeder returns a seeder talking to the engine at socket.
func NewSeeder(socket string) *Seeder {
	return &Seeder{api: docker.New(socket)}
}

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
func (s *Seeder) Seed(ctx context.Context, container string, g *manifest.Game, host string, secrets *RunSecrets) error {
	rendered, err := RenderTemplates(g, host, secrets)
	if err != nil {
		return err
	}

	if len(rendered) > 0 {
		archive, err := tarEntries(rendered)
		if err != nil {
			return fmt.Errorf("pack rendered files: %w", err)
		}
		if err := s.api.PutArchive(ctx, container, "/", bytes.NewReader(archive)); err != nil {
			return fmt.Errorf("upload rendered files: %w", err)
		}
	}

	if err := s.setPasswords(ctx, container, g, host, secrets); err != nil {
		return err
	}
	return s.restoreShadowTimes(ctx, container, g)
}

// restoreShadowTimes ages the account databases back after chpasswd.
//
// Seeding happens at container start, which is the real present, not the
// fictional one. Left alone, /etc/shadow would be dated today on a box whose
// logs stop months ago -- and it would be dated differently for every player,
// which is worse: it times the container's creation to the minute.
func (s *Seeder) restoreShadowTimes(ctx context.Context, container string, g *manifest.Game) error {
	aging := NewAging(g.ID, g.Version, g.Timeline.Start, g.Timeline.Span)
	owner := Owner{User: "root"}

	// touch takes a single -d for the whole invocation, so each file needs its
	// own call; one shell loop is cheaper than one exec per file.
	var script strings.Builder
	for _, name := range accountDatabases {
		at := aging.FileTime(owner, name)
		fmt.Fprintf(&script, "[ -e %s ] && touch -d @%d %s\n", name, at.Unix(), name)
	}
	script.WriteString("exit 0\n")

	res, err := s.api.Exec(ctx, container, docker.ExecOptions{
		Cmd:  []string{"sh", "-c", script.String()},
		User: "root",
	})
	if err != nil {
		return fmt.Errorf("age account databases: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("aging account databases exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// setPasswords writes the level accounts' passwords via chpasswd on stdin.
//
// The passwords never touch the container's filesystem except as hashes in
// /etc/shadow, which is where a player would expect to find them and where they
// are of no use: the hash is yescrypt over 24 characters of derived entropy.
func (s *Seeder) setPasswords(ctx context.Context, container string, g *manifest.Game, host string, secrets *RunSecrets) error {
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

	res, err := s.api.Exec(ctx, container, docker.ExecOptions{
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

// RenderTemplates finds every templated file in a game and renders it against a
// run's secrets, returning entries ready to be uploaded.
//
// The aging plan is recomputed here rather than carried in the image. It is a
// pure function of the game id, version and path, so the seeder can restore the
// exact mtime a rendered file is supposed to have -- without which every file
// holding a credential would carry the container's start time and stand out
// from everything around it.
func RenderTemplates(g *manifest.Game, host string, secrets *RunSecrets) ([]entry, error) {
	accounts, err := planAccounts(g)
	if err != nil {
		return nil, err
	}
	aging := NewAging(g.ID, g.Version, g.Timeline.Start, g.Timeline.Span)

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
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if !bytes.Contains(raw, []byte(templateMarker)) {
			return nil
		}

		rel, err := filepath.Rel(root, src)
		if err != nil {
			return err
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

// SeedTimeout bounds seeding. A container that cannot be seeded must not be
// handed to a player: they would find placeholders where the game should be.
const SeedTimeout = 30 * time.Second
