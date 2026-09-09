// Package manifest defines the game authoring format and loads it from disk.
//
// A game is a directory: game.yaml describing the image and the level graph,
// and one subdirectory per level holding that level's metadata, home directory
// tree and setup script. The engine compiles that into an image; nothing in
// here knows about containers.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// GameFile is the manifest at the root of a game directory.
const GameFile = "game.yaml"

// LevelsDir holds one subdirectory per level.
const LevelsDir = "levels"

// LevelFile is the manifest inside a level directory.
const LevelFile = "level.yaml"

// HomeDir inside a level directory is copied verbatim into the level user's
// home directory.
const HomeDir = "home"

// FilesDir inside a level directory is copied to absolute paths in the image:
// files/var/backups/rota.db becomes /var/backups/rota.db. Everything in it is
// owned by the level's account, so that a level's artifacts outside its home
// directory still belong to it.
const FilesDir = "files"

// SetupScript inside a level directory runs at image build time, after the home
// tree is copied, to fix permissions and install services and cron entries.
const SetupScript = "setup.sh"

// Game is a complete, loaded game definition.
type Game struct {
	ID       string   `yaml:"id"`
	Version  int      `yaml:"version"`
	Title    string   `yaml:"title"`
	Synopsis string   `yaml:"synopsis"`
	Base     string   `yaml:"base"`
	Packages []string `yaml:"packages"`

	Timeline Timeline `yaml:"timeline"`

	// NoiseUsers are decoy accounts with their own mail, history and abandoned
	// files. Without them /home is a table of contents: as many home
	// directories as levels tells a player the whole shape of the game.
	NoiseUsers int `yaml:"noise_users"`

	// Hosts is the set of machines a run spans. Single-host games may omit it
	// entirely and every level lands on an implicit default host.
	Hosts []*Host `yaml:"hosts"`

	// Levels is populated by Load from the levels/ subdirectory, not from
	// game.yaml, so that adding a level is a matter of adding a directory.
	Levels []*Level `yaml:"-"`

	// Dir is the directory the game was loaded from.
	Dir string `yaml:"-"`
}

// Timeline anchors the fictional clock. Files are aged around it at build time:
// an image whose every mtime is the build timestamp reads as fake immediately.
type Timeline struct {
	Start time.Time `yaml:"start"`

	// Span is how long the box's fictional history runs. The present -- the
	// newest any file on the box may be -- is Start plus Span. Zero means the
	// engine's default.
	Span time.Duration `yaml:"span"`
}

// Host is one machine in a run. A run of a multi-host game is a set of
// containers on a private bridge network.
type Host struct {
	ID       string   `yaml:"id"`
	Base     string   `yaml:"base"`
	Packages []string `yaml:"packages"`

	// Egress lists the other hosts this one may reach. Anything not listed is
	// dropped, as is all traffic leaving the run.
	Egress []string `yaml:"egress"`
}

// DefaultHostID is the implicit host of a single-host game.
const DefaultHostID = "main"

// Level is one rung of the game: a Unix user, the contents of its home
// directory, and the credential it leads to.
type Level struct {
	ID   string `yaml:"id"`
	User string `yaml:"user"`
	Host string `yaml:"host"`

	// Requires lists the levels whose credentials open this one. Empty marks
	// the entry level. More than one entry is how split credentials work: an
	// SSH key found on one level, its passphrase on another.
	Requires []string `yaml:"requires"`

	// Grants are the credentials this level leaves behind for others.
	Grants []Grant `yaml:"grants"`

	Narrative Narrative `yaml:"narrative"`
	Services  []Service `yaml:"services"`

	// Dir is the level's directory on disk.
	Dir string `yaml:"-"`
}

// GrantKind is the sort of credential a level hands onward.
type GrantKind string

const (
	// GrantPassword is a login password for the target level's user.
	GrantPassword GrantKind = "password"
	// GrantSSHKey is the target user's private key.
	GrantSSHKey GrantKind = "ssh-key"
	// GrantPassphrase unlocks a private key granted elsewhere.
	GrantPassphrase GrantKind = "key-passphrase"
)

// Grant is a credential placed somewhere in this level's reachable filesystem
// that opens another level.
type Grant struct {
	Kind GrantKind `yaml:"kind"`
	To   string    `yaml:"to"`

	// PlacedIn is where the credential can be found. It is not documentation:
	// the validator reads this path as the level's user to prove the level is
	// solvable, and treats it as the target the hint engine watches for.
	//
	// A path may name a member inside an archive, separated by a colon:
	//   /var/backups/nightly.tar.gz:etc/shadow.bak
	PlacedIn string `yaml:"placed_in"`
}

// Path returns the filesystem path of the grant, without any archive member.
func (g Grant) Path() string {
	path, _, _ := strings.Cut(g.PlacedIn, ":")
	return path
}

// Member returns the archive member the credential sits in, if any.
func (g Grant) Member() string {
	_, member, _ := strings.Cut(g.PlacedIn, ":")
	return member
}

// Narrative is the in-world material that carries the story. There is
// deliberately no out-of-world channel here: hints reach the player as mail
// from a correspondent, not as a command that would have no business existing
// on the machine.
type Narrative struct {
	// Mail are message files delivered to the level user's mailbox.
	Mail []string `yaml:"mail"`
	// MOTD is shown on login to this level.
	MOTD string `yaml:"motd"`
	// Correspondents can be written to for help; each reply tier escalates
	// from a nudge toward the answer.
	Correspondents []Correspondent `yaml:"correspondents"`
}

// Correspondent is an in-fiction character the player can mail.
type Correspondent struct {
	Address string `yaml:"address"`
	Name    string `yaml:"name"`
	// Replies are the escalating hint tiers, in order.
	Replies []string `yaml:"replies"`
	// Unprompted is sent when the player has stalled on this level long
	// enough that a handler would reasonably check in.
	Unprompted string `yaml:"unprompted"`
	// StallAfter is how long a player may go without progress before
	// Unprompted is sent. Zero disables it.
	StallAfter time.Duration `yaml:"stall_after"`
}

// Service is a daemon the game runs. Services give a box a pulse and are where
// the better puzzles live, but they are also privileged reachable surface: the
// validator folds each service account into the permission graph of every level
// that can reach it.
type Service struct {
	Name string `yaml:"name"`
	// User is the account the service runs as. Never root: a service running
	// as root hands its own uid to whoever compromises it, which collapses
	// every level above it at once.
	User string `yaml:"user"`
	Unit string `yaml:"unit"`
	Port int    `yaml:"port"`
}

var (
	slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	userPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
)

// Load reads and validates the game rooted at dir.
func Load(dir string) (*Game, error) {
	raw, err := os.ReadFile(filepath.Join(dir, GameFile))
	if err != nil {
		return nil, fmt.Errorf("read game manifest: %w", err)
	}

	var g Game
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // a typo in a manifest key must fail, not be ignored
	if err := dec.Decode(&g); err != nil {
		return nil, fmt.Errorf("%s: %w", GameFile, err)
	}
	g.Dir = dir

	if err := g.loadLevels(); err != nil {
		return nil, err
	}
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return &g, nil
}

func (g *Game) loadLevels() error {
	root := filepath.Join(g.Dir, LevelsDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read levels directory: %w", err)
	}

	// Directory names are sorted so that a numeric prefix (01-mailroom) gives
	// authors a stable, readable ordering on disk. The graph, not this order,
	// determines progression.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, LevelFile))
		if err != nil {
			return fmt.Errorf("read level manifest in %s: %w", e.Name(), err)
		}

		var l Level
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(&l); err != nil {
			return fmt.Errorf("%s/%s: %w", e.Name(), LevelFile, err)
		}
		l.Dir = dir
		if l.Host == "" {
			l.Host = DefaultHostID
		}
		g.Levels = append(g.Levels, &l)
	}

	if len(g.Levels) == 0 {
		return fmt.Errorf("game %q has no levels", g.ID)
	}
	return nil
}

// Level returns the level with the given id.
func (g *Game) Level(id string) (*Level, bool) {
	for _, l := range g.Levels {
		if l.ID == id {
			return l, true
		}
	}
	return nil, false
}

// LevelIDs returns every level id, in manifest order.
func (g *Game) LevelIDs() []string {
	ids := make([]string, 0, len(g.Levels))
	for _, l := range g.Levels {
		ids = append(ids, l.ID)
	}
	return ids
}

// Entry returns the level a player starts on.
func (g *Game) Entry() (*Level, bool) {
	for _, l := range g.Levels {
		if len(l.Requires) == 0 {
			return l, true
		}
	}
	return nil, false
}

// ReceivesKey reports whether a level is opened by an SSH key placed somewhere
// in the game, rather than by a password.
func (g *Game) ReceivesKey(levelID string) bool {
	for _, l := range g.Levels {
		for _, gr := range l.Grants {
			if gr.To == levelID && gr.Kind == GrantSSHKey {
				return true
			}
		}
	}
	return false
}

// HostIDs returns every declared host, or the implicit default.
func (g *Game) HostIDs() []string {
	if len(g.Hosts) == 0 {
		return []string{DefaultHostID}
	}
	ids := make([]string, 0, len(g.Hosts))
	for _, h := range g.Hosts {
		ids = append(ids, h.ID)
	}
	return ids
}
