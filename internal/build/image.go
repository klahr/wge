package build

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klahr/wge/internal/docker"
	"github.com/klahr/wge/internal/manifest"
)

// Builder compiles a game directory into a container image.
type Builder struct {
	api *docker.Client
}

// NewBuilder returns a builder talking to the engine at socket.
func NewBuilder(socket string) *Builder {
	return &Builder{api: docker.New(socket)}
}

// Result describes what a build produced.
type Result struct {
	Image    string
	Files    int
	Accounts int
	Oldest   time.Time
	Newest   time.Time
}

// Build assembles and builds the image for one host of a game.
func (b *Builder) Build(ctx context.Context, g *manifest.Game, host, tag string, progress io.Writer) (*Result, error) {
	anchor := ResolveAnchor(g.Timeline.Start, g.Timeline.Span.Duration(), time.Now())

	plan, err := Assemble(g, host, anchor)
	if err != nil {
		return nil, err
	}

	archive, err := plan.Context()
	if err != nil {
		return nil, fmt.Errorf("assemble build context: %w", err)
	}

	if err := b.api.Build(ctx, tag, bytes.NewReader(archive), progress); err != nil {
		return nil, err
	}

	oldest, newest := plan.Bounds()
	return &Result{
		Image:    tag,
		Files:    len(plan.rootfs.entries),
		Accounts: len(plan.Accounts),
		Oldest:   oldest,
		Newest:   newest,
	}, nil
}

// Plan is a fully assembled image, ready to be turned into a build context.
type Plan struct {
	Game     *manifest.Game
	Host     string
	Accounts []*account
	Aging    *Aging

	// Anchor is the resolved start of the fictional timeline. It is recorded on
	// the image so that seeding and verification age against the same clock the
	// build did.
	Anchor time.Time

	rootfs    *rootfs
	setup     map[string][]byte // level id -> setup.sh
	gameSetup []byte            // the game's own setup.sh, if it has one
}

// Assemble builds the plan for one host of a game without touching Docker, so
// it can be inspected and tested on its own.
func Assemble(g *manifest.Game, host string, anchor time.Time) (*Plan, error) {
	accounts, err := planAccounts(g, host)
	if err != nil {
		return nil, err
	}

	p := &Plan{
		Game:     g,
		Host:     host,
		Accounts: accounts,
		Anchor:   anchor,
		Aging:    NewAging(g.ID, g.Version, anchor, g.Timeline.Span.Duration()),
		rootfs:   newRootfs(),
		setup:    map[string][]byte{},
	}

	for _, acct := range accounts {
		if err := p.addAccount(acct); err != nil {
			return nil, err
		}
	}
	if err := p.addHostKeys(); err != nil {
		return nil, err
	}
	if err := p.addServices(); err != nil {
		return nil, err
	}
	p.addCron()

	if err := p.addSystemFiles(); err != nil {
		return nil, err
	}

	if err := p.loadSetupScripts(); err != nil {
		return nil, err
	}
	return p, nil
}

// addAccount lays down one account's home directory: the author's files where
// there are any, and generated ones where there are not.
func (p *Plan) addAccount(acct *account) error {
	owner := Owner{User: acct.User}

	p.rootfs.add(entry{
		Path: acct.Home, Dir: true, Mode: 0o750,
		UID: acct.UID, GID: acct.GID,
		ModTime: p.Aging.AccountCreated(owner),
	})

	if acct.Level != nil {
		if err := p.copyTree(acct, manifest.HomeDir, acct.Home); err != nil {
			return err
		}
		if err := p.copyTree(acct, manifest.FilesDir, "/"); err != nil {
			return err
		}
		if err := p.deliverMail(acct); err != nil {
			return err
		}
	}

	// The skeleton dotfiles are copied in by provision.sh, so they are not in
	// the tar and would otherwise keep the build timestamp -- three files
	// dated today in a home directory that is otherwise months old.
	for _, name := range skeletonFiles {
		p.rootfs.addMeta(path.Join(acct.Home, name), p.Aging.AccountCreated(owner))
	}

	// Decoys need enough of a life to survive being looked at.
	if !acct.Service && !p.rootfs.has(path.Join(acct.Home, ".bash_history")) {
		lines := 12 + int(p.Aging.fraction("histlen", acct.User)*40)
		at := p.Aging.FileTime(owner, path.Join(acct.Home, ".bash_history"))
		p.rootfs.addFile(path.Join(acct.Home, ".bash_history"),
			bashHistory(p.Aging, acct, lines), 0o600, acct, at)
	}

	return nil
}

// copyTree copies one of a level's authored trees into the image under base,
// aging every file as it goes.
func (p *Plan) copyTree(acct *account, tree, base string) error {
	root := filepath.Join(acct.Level.Dir, tree)
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	}

	owner := Owner{User: acct.User}

	return filepath.WalkDir(root, func(src string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, src)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		dst := path.Join(base, filepath.ToSlash(rel))

		info, err := d.Info()
		if err != nil {
			return err
		}

		if d.IsDir() {
			p.rootfs.add(entry{
				Path: dst, Dir: true, Mode: 0o755,
				UID: acct.UID, GID: acct.GID,
				ModTime: p.Aging.FileTime(owner, dst),
			})
			return nil
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(src)
			if err != nil {
				return err
			}
			p.rootfs.add(entry{
				Path: dst, Link: target, Mode: 0o777,
				UID: acct.UID, GID: acct.GID,
				ModTime: p.Aging.FileTime(owner, dst),
			})
			return nil
		}

		content, err := os.ReadFile(src)
		if err != nil {
			return err
		}

		// The author's mode is preserved: a level that hinges on a file being
		// group-readable must be able to say so by chmod-ing it on disk.
		mode := info.Mode().Perm()
		p.rootfs.addFile(dst, content, mode, acct, p.Aging.FileTime(owner, dst))
		return nil
	})
}

// deliverMail assembles a level user's mailbox.
//
// Mail is the best narrative vehicle a Unix box has: reading other people's
// correspondence is already the genre, and a mailbox needs no explanation for
// being there.
func (p *Plan) deliverMail(acct *account) error {
	if len(acct.Level.Narrative.Mail) == 0 {
		return nil
	}

	var messages []string
	for _, rel := range acct.Level.Narrative.Mail {
		raw, err := os.ReadFile(filepath.Join(acct.Level.Dir, rel))
		if err != nil {
			return fmt.Errorf("level %s: read mail %s: %w", acct.Level.ID, rel, err)
		}
		messages = append(messages, string(raw))
	}

	mbox := path.Join("/var/mail", acct.User)
	p.rootfs.add(entry{
		Path: mbox, Content: mailbox(messages),
		// Mail spools are group-mail and mode 0660 on a Debian system. A
		// world-readable mailbox would hand every level's mail to level one.
		Mode: 0o660, UID: acct.UID, GID: acct.GID,
		ModTime: p.Aging.LogTime("mail/"+acct.User, 9, 10),
	})
	return nil
}

// addSystemFiles writes the box's shared memory of itself: who has logged in,
// when, and from where.
//
// Both generations of login database are written. Debian 13 replaced the binary
// wtmp and lastlog with SQLite ones under wtmpdb and lastlog2, and an image
// built on an older base still reads the binary pair -- so producing only one
// of them means producing a login history that, on half the bases an author
// might pick, nothing on the box can read.
func (p *Plan) addSystemFiles() error {
	root := &account{User: "root", UID: 0, GID: 0}
	sessions := p.Aging.sessions(p.Accounts)

	p.rootfs.add(entry{
		Path: "/var/log/wtmp", Content: wtmp(p.Aging, sessions),
		Mode: 0o664, UID: 0, GID: 43, // root:utmp
		ModTime: p.Aging.LogTime("wtmp", 9, 10),
	})
	p.rootfs.add(entry{
		Path: "/var/log/lastlog", Content: lastlog(p.Accounts, sessions),
		Mode: 0o664, UID: 0, GID: 43,
		ModTime: p.Aging.LogTime("lastlog", 9, 10),
	})

	wtmpdb, err := wtmpdbDatabase(p.Aging, sessions)
	if err != nil {
		return fmt.Errorf("generate wtmpdb: %w", err)
	}
	p.rootfs.add(entry{
		Path: wtmpdbPath, Content: wtmpdb,
		Mode: 0o644, UID: 0, GID: 0,
		ModTime: p.Aging.LogTime("wtmpdb", 9, 10),
	})
	p.rootfs.add(entry{
		Path: wtmpdbLinkPath, Link: wtmpdbLinkDest,
		Mode: 0o777, UID: 0, GID: 0,
		ModTime: p.Aging.LogTime("wtmpdb", 9, 10),
	})

	lastlog2, err := lastlog2Database(sessions)
	if err != nil {
		return fmt.Errorf("generate lastlog2: %w", err)
	}
	p.rootfs.add(entry{
		Path: lastlog2Path, Content: lastlog2,
		Mode: 0o644, UID: 0, GID: 0,
		ModTime: p.Aging.LogTime("lastlog2", 9, 10),
	})

	// rsyslog appends to this; without a history it would begin at the moment
	// this player's container started.
	p.rootfs.add(entry{
		Path: "/var/log/syslog", Content: p.syslogHistory(),
		Mode: 0o640, UID: 0, GID: 4,
		ModTime: p.Aging.LogTime("syslog", 9, 10),
	})

	p.rootfs.add(entry{
		Path: "/var/log/auth.log", Content: authLog(p.Aging, p.Accounts, sessions, p.Host),
		// Debian keeps auth.log unreadable by ordinary users. A world-readable
		// one would hand a player every account name on the box for free.
		Mode: 0o640, UID: 0, GID: 4,
		ModTime: p.Aging.LogTime("auth.log", 9, 10),
	})

	// useradd rewrites the account databases during the build, so every one of
	// them carries the build time. A passwd file modified today on a box whose
	// logs stop months ago is as loud a tell as there is.
	for _, name := range accountDatabases {
		p.rootfs.addMeta(name, p.Aging.FileTime(Owner{User: "root"}, name))
	}

	if _, ok := p.hostMOTD(); ok {
		p.rootfs.addFile("/etc/motd", []byte(p.motd()), 0o644, root,
			p.Aging.FileTime(Owner{User: "root"}, "/etc/motd"))
	}
	return nil
}

// hostMOTD is the banner this machine greets a login with.
//
// It has to come from a level that lives here. Putting the entry level's motd
// on every host would have the document store introduce itself as the mail
// relay, which is a contradiction on the first line a player reads.
func (p *Plan) hostMOTD() (string, bool) {
	if entry, ok := p.Game.Entry(); ok && entry.Host == p.Host &&
		strings.TrimSpace(entry.Narrative.MOTD) != "" {
		return entry.Narrative.MOTD, true
	}
	for _, l := range p.Game.Levels {
		if l.Host == p.Host && strings.TrimSpace(l.Narrative.MOTD) != "" {
			return l.Narrative.MOTD, true
		}
	}
	return "", false
}

func (p *Plan) motd() string {
	text, _ := p.hostMOTD()
	return strings.TrimRight(text, "\n") + "\n"
}

func (p *Plan) loadSetupScripts() error {
	// The machine's own configuration, for the things that belong to no
	// single level.
	raw, err := os.ReadFile(filepath.Join(p.Game.Dir, manifest.SetupScript))
	if err == nil {
		p.gameSetup = raw
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read game %s: %w", manifest.SetupScript, err)
	}

	for _, l := range p.Game.Levels {
		if l.Host != p.Host {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(l.Dir, manifest.SetupScript))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("level %s: read %s: %w", l.ID, manifest.SetupScript, err)
		}
		p.setup[l.ID] = raw
	}
	return nil
}

// skeletonFiles are the dotfiles /etc/skel gives a new account.
var skeletonFiles = []string{".bashrc", ".bash_logout", ".profile"}

// accountDatabases are rewritten by useradd during the build and must be aged
// back afterwards.
var accountDatabases = []string{
	"/etc/passwd", "/etc/passwd-", "/etc/shadow", "/etc/shadow-",
	"/etc/group", "/etc/group-", "/etc/gshadow", "/etc/gshadow-",
	"/etc/subuid", "/etc/subgid",
}

// ModTime returns the timestamp the plan assigns to a path.
func (p *Plan) ModTime(target string) (time.Time, bool) {
	for _, e := range p.rootfs.all() {
		if e.Path == target {
			return e.ModTime, true
		}
	}
	return time.Time{}, false
}

// ScratchPath is where a run's persistent scratch space is mounted.
//
// A shared scratch directory is a thing corporate machines actually have, which
// is what makes it a plausible place for the one part of the filesystem that
// survives a container being reaped. It is deliberately not inside anybody's
// home directory: it follows the player across accounts and across machines,
// and a home directory that did that would be a stranger thing to explain.
const ScratchPath = "/srv/scratch"

// ScratchGroup owns the scratch directory. Every account a player can log in
// as is a member, which is what lets a note written under one level be edited
// under the next.
const ScratchGroup = "scratch"

// hasServices reports whether any level on this host declares a service.
func (p *Plan) hasServices() bool {
	for _, l := range p.Game.Levels {
		if l.Host == p.Host && len(l.Services) > 0 {
			return true
		}
	}
	return false
}

// PathsByLevel returns the paths each level's account owns, which is what the
// permission verification compares against: a level must not be able to read
// what belongs to a level it has not earned its way to.
func (p *Plan) PathsByLevel() map[string][]string {
	levelByUID := map[int]string{}
	for _, a := range p.Accounts {
		if a.Level != nil {
			levelByUID[a.UID] = a.Level.ID
		}
	}

	owned := map[string][]string{}
	for _, e := range p.rootfs.all() {
		// MetaOnly entries are files the build created rather than files the
		// game placed; they belong to the system, not to a level.
		if e.MetaOnly || e.UID < 0 {
			continue
		}
		if level, ok := levelByUID[e.UID]; ok {
			owned[level] = append(owned[level], e.Path)
		}
	}
	for level := range owned {
		sort.Strings(owned[level])
	}
	return owned
}

// Bounds returns the oldest and newest mtime in the plan, which is what makes
// the aging visible in a build's output.
func (p *Plan) Bounds() (oldest, newest time.Time) {
	for i, e := range p.rootfs.all() {
		if i == 0 || e.ModTime.Before(oldest) {
			oldest = e.ModTime
		}
		if i == 0 || e.ModTime.After(newest) {
			newest = e.ModTime
		}
	}
	return oldest, newest
}

// Context renders the plan as a Docker build context tarball.
func (p *Plan) Context() ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	add := func(name string, content []byte, mode int64) error {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: mode, Size: int64(len(content)),
			Typeflag: tar.TypeReg, ModTime: p.Game.Timeline.Start,
			Format: tar.FormatPAX,
		}); err != nil {
			return err
		}
		_, err := tw.Write(content)
		return err
	}

	if err := add("Dockerfile", []byte(p.dockerfile()), 0o644); err != nil {
		return nil, err
	}
	if err := add("provision.sh", []byte(p.provisionScript()), 0o755); err != nil {
		return nil, err
	}
	if err := add("apply-meta.sh", []byte(applyMetaScript), 0o755); err != nil {
		return nil, err
	}
	if err := add("meta.txt", p.rootfs.metaPlan(), 0o644); err != nil {
		return nil, err
	}
	if err := add("sysdirs.txt",
		[]byte(strings.Join(p.rootfs.systemDirsTouched(), "\n")+"\n"), 0o644); err != nil {
		return nil, err
	}
	if err := add("setup.sh", []byte(p.setupScript()), 0o755); err != nil {
		return nil, err
	}
	if p.hasServices() {
		if err := add("services.sh", []byte(p.registerServices()), 0o755); err != nil {
			return nil, err
		}
	}
	if len(p.gameSetup) > 0 {
		if err := add("game-setup.sh", p.gameSetup, 0o755); err != nil {
			return nil, err
		}
	}
	for id, script := range p.setup {
		if err := add("setup/"+id+".sh", script, 0o755); err != nil {
			return nil, err
		}
	}
	if err := p.rootfs.writeTar(tw, "rootfs"); err != nil {
		return nil, err
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// dockerfile renders the build.
//
// The order is load-bearing. Accounts must exist before their files are copied,
// so the copied files land with the right ownership. The aging fix-up must run
// last, because every step in the middle re-stamps whatever it touches with the
// build time -- which is the exact tell the aging exists to remove.
func (p *Plan) dockerfile() string {
	var b strings.Builder

	fmt.Fprintf(&b, "FROM %s\n", p.base())
	b.WriteString("ENV DEBIAN_FRONTEND=noninteractive\n")

	if pkgs := p.packages(); len(pkgs) > 0 {
		// The package manager's own logs are the loudest tell on the box.
		// history.log keeps the literal apt-get command line -- which reads
		// like a game engine's recipe -- and dpkg.log dates every install to
		// the minute the image was built.
		fmt.Fprintf(&b, "RUN apt-get update \\\n"+
			" && apt-get install -y --no-install-recommends %s \\\n"+
			" && rm -rf /var/lib/apt/lists/* /var/log/apt /var/log/dpkg.log \\\n"+
			"      /var/log/alternatives.log /var/cache/debconf/*.dat-old\n",
			strings.Join(pkgs, " "))
	}

	b.WriteString("COPY provision.sh /wge/provision.sh\n")
	b.WriteString("RUN sh /wge/provision.sh\n")

	b.WriteString("COPY apply-meta.sh meta.txt sysdirs.txt /wge/\n")

	// Record what the system directories look like before COPY disturbs them.
	b.WriteString("RUN sh /wge/apply-meta.sh /wge/meta.txt snapshot\n")

	b.WriteString("COPY rootfs/ /\n")

	// And put them back.
	b.WriteString("RUN sh /wge/apply-meta.sh /wge/meta.txt restore\n")

	// Ownership first, so a level's setup.sh runs against files that already
	// belong to the right accounts and can express its access control on top.
	b.WriteString("RUN sh /wge/apply-meta.sh /wge/meta.txt own\n")

	// Services are registered before the level setup scripts, so a setup script
	// can disable or reorder one if the game wants it to.
	if p.hasServices() {
		b.WriteString("COPY services.sh /wge/services.sh\n")
		b.WriteString("RUN sh /wge/services.sh\n")
	}

	if len(p.gameSetup) > 0 {
		b.WriteString("COPY game-setup.sh /wge/game-setup.sh\n")
		b.WriteString("RUN sh /wge/game-setup.sh\n")
	}

	if len(p.setup) > 0 {
		b.WriteString("COPY setup.sh /wge/setup.sh\n")
		b.WriteString("COPY setup/ /wge/setup/\n")
		b.WriteString("RUN sh /wge/setup.sh\n")
	}

	// Timestamps last, and only timestamps: everything above this line has
	// re-stamped whatever it touched with the build time.
	//
	// The sweep comes first and covers everything the game did not place --
	// the base image's own files, and whatever the package installs dropped --
	// then the plan gives the game's own files their individual dates.
	fmt.Fprintf(&b, "RUN sh /wge/apply-meta.sh /wge/meta.txt sweep %d \\\n"+
		" && sh /wge/apply-meta.sh /wge/meta.txt time \\\n"+
		" && rm -rf /wge\n", p.Aging.Provisioned().Unix())

	// The anchor travels as an image label: invisible from inside the
	// container, and available to everything downstream that has to age
	// against the same clock the build used.
	fmt.Fprintf(&b, "LABEL %s=%d\n", AnchorLabel, p.Anchor.Unix())

	// A real init, so the box has a real process tree, services have somewhere
	// to be started from, and orphaned processes are reaped by something that
	// is meant to reap them.
	b.WriteString(`CMD ["/sbin/init"]` + "\n")

	return b.String()
}

func (p *Plan) base() string {
	for _, h := range p.Game.Hosts {
		if h.ID == p.Host && h.Base != "" {
			return h.Base
		}
	}
	return p.Game.Base
}

func (p *Plan) packages() []string {
	seen := map[string]bool{}
	var out []string

	for _, pkg := range p.Game.Packages {
		if !seen[pkg] {
			seen[pkg] = true
			out = append(out, pkg)
		}
	}
	for _, h := range p.Game.Hosts {
		if h.ID != p.Host {
			continue
		}
		for _, pkg := range h.Packages {
			if !seen[pkg] {
				seen[pkg] = true
				out = append(out, pkg)
			}
		}
	}
	sort.Strings(out)
	return out
}

// provisionScript creates every account with a fixed uid, so that the tar's
// numeric ownership lines up with the accounts the image ends up having.
func (p *Plan) provisionScript() string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -eu\n\n")

	for _, a := range p.Accounts {
		system := ""
		if a.Service {
			system = "--system "
		}
		fmt.Fprintf(&b, "groupadd --gid %d %s\n", a.GID, a.User)
		fmt.Fprintf(&b, "useradd %s--uid %d --gid %d --home-dir %s --shell %s --comment %q %s\n",
			system, a.UID, a.GID, a.Home, a.Shell, a.Gecos, a.User)
	}

	// Home directories arrive in the rootfs copy, but the skeleton files a real
	// account gets have to come from somewhere.
	b.WriteString("\nfor home in")
	for _, a := range p.Accounts {
		if !a.Service {
			fmt.Fprintf(&b, " %s", a.Home)
		}
	}
	b.WriteString("; do\n")
	b.WriteString("  mkdir -p \"$home\"\n")
	b.WriteString("  cp -rn /etc/skel/. \"$home\"/ 2>/dev/null || true\n")
	b.WriteString("done\n\n")

	for _, a := range p.Accounts {
		if !a.Service {
			fmt.Fprintf(&b, "chown -R %d:%d %s\n", a.UID, a.GID, a.Home)
			fmt.Fprintf(&b, "chmod 750 %s\n", a.Home)
		}
	}

	// Mail spools must exist for the levels that have one, with the group a
	// Debian system expects.
	b.WriteString("\nmkdir -p /var/mail\nchmod 2775 /var/mail\n")

	// The shared scratch mount, done the way a shared project directory is done
	// on any Unix box: a group everybody who uses it belongs to, setgid so new
	// files inherit that group, and a umask that leaves them group-writable.
	//
	// The obvious thing -- copying /tmp's 1777 -- fails twice over. The kernel's
	// fs.protected_regular refuses to open a file owned by another user with
	// O_CREAT inside a world-writable sticky directory, and `>>` passes O_CREAT,
	// so appending to a note written under another account is denied outright.
	// Dropping the sticky bit gets past that and straight into the ordinary
	// permission: a file created with the default umask is 0664 owned by its
	// author's private group, which no other account is in.
	//
	// Both of those exist to protect users from each other. Inside one run
	// every account is the same person.
	fmt.Fprintf(&b, "\ngroupadd -f %s\n", ScratchGroup)
	for _, a := range p.Accounts {
		if !a.Service {
			fmt.Fprintf(&b, "usermod -a -G %s %s\n", ScratchGroup, a.User)
		}
	}
	fmt.Fprintf(&b, "mkdir -p %s\nchgrp %s %s\nchmod 2775 %s\n",
		ScratchPath, ScratchGroup, ScratchPath, ScratchPath)

	// pam_umask reads this, so it applies to every login rather than only to
	// shells that happen to source /etc/profile.
	b.WriteString("sed -i 's/^UMASK.*/UMASK\t\t002/' /etc/login.defs\n")

	return b.String()
}

// setupScript runs each level's setup.sh in a predictable order.
func (p *Plan) setupScript() string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -eu\n\n")

	ids := make([]string, 0, len(p.setup))
	for id := range p.setup {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		fmt.Fprintf(&b, "echo '--- setup: %s'\n", id)
		fmt.Fprintf(&b, "sh /wge/setup/%s.sh\n", id)
	}
	return b.String()
}

// applyMetaScript applies ownership, permissions and timestamps as the build's
// final act. It runs last because everything before it -- COPY discarding tar
// ownership, setup scripts, package hooks -- has left both wrong.
const applyMetaScript = `#!/bin/sh
# Apply one phase of the metadata plan.
#
#   snapshot -- record the mode and ownership of the system directories the
#            game places files into, before COPY rewrites them.
#   restore  -- put those back afterwards. COPY resets the metadata of every
#            directory it traverses, existing or not, and /var/mail must stay
#            2775 root:mail or mail(1) cannot lock a mailbox.
#   own   -- restore ownership, which COPY discarded. Runs before the level
#            setup scripts so they can override it and build on it.
#   sweep -- date everything the game did not place to the provisioning time,
#            leaving alone anything already older (package files carry genuine
#            upstream dates, and those are more plausible than any invention).
#            This catches files. Docker's layer diff drops a directory whose
#            only change is its mtime, so empty directories are swept again at
#            container start, where there is no layer diff to lose them.
#   time  -- restore mtimes for the paths the plan names. Runs last, after
#            everything that would re-stamp them with the build time.
#
# Permissions are not in the plan: the tar already carries the author's mode,
# and setup.sh owns any change to it.
set -u

plan="$1"
phase="$2"

if [ "$phase" = "snapshot" ]; then
	: > /wge/sysdirs.saved
	while IFS= read -r dir; do
		[ -d "$dir" ] || continue
		stat -c '%a %u %g %n' -- "$dir" >> /wge/sysdirs.saved
	done < /wge/sysdirs.txt
	exit 0
fi

if [ "$phase" = "restore" ]; then
	while read -r mode uid gid dir; do
		[ -d "$dir" ] || continue
		chown "$uid:$gid" -- "$dir"
		chmod "$mode" -- "$dir"
	done < /wge/sysdirs.saved
	exit 0
fi

if [ "$phase" = "sweep" ]; then
	provisioned="$3"

	cut -f3 "$plan" | sort -u > /wge/planned
	find / -xdev -newermt "@$provisioned" \
		-not -path '/proc/*' -not -path '/sys/*' -not -path '/dev/*' \
		-not -path '/wge*' 2>/dev/null | sort -u > /wge/newer

	# Everything newer than the provisioning date that the plan does not name.
	comm -23 /wge/newer /wge/planned > /wge/stamp
	xargs -a /wge/stamp -d '\n' -r touch -h -d "@$provisioned" -- 2>/dev/null

	rm -f /wge/planned /wge/newer /wge/stamp
	exit 0
fi

while IFS='	' read -r owner ts target; do
	[ -n "$ts" ] || continue
	if [ ! -e "$target" ] && [ ! -L "$target" ]; then
		continue
	fi

	case "$phase" in
	own)
		[ "$owner" = "-:-" ] || chown -h "$owner" -- "$target" 2>/dev/null || true
		;;
	time)
		touch -h -d "@$ts" -- "$target" 2>/dev/null || true
		;;
	esac
done < "$plan"
`
