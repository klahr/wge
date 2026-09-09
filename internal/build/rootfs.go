package build

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// entry is one filesystem object destined for the image.
//
// UID of -1 means "leave whatever owns it alone", and a zero Mode means the
// same for permissions. Both are used by entries that only exist to carry a
// timestamp for a file the build itself created.
type entry struct {
	Path     string // absolute path inside the image
	Mode     fs.FileMode
	UID, GID int
	Content  []byte
	Link     string // symlink target, when Mode says symlink
	Dir      bool
	ModTime  time.Time

	// MetaOnly entries appear in the metadata plan but are not shipped in the
	// tar: the file already exists in the image, put there by a package or by
	// useradd, and only its metadata needs correcting.
	MetaOnly bool
}

// rootfs is the filesystem the build lays over the base image.
type rootfs struct {
	entries []entry
	index   map[string]int
}

func newRootfs() *rootfs {
	return &rootfs{index: map[string]int{}}
}

// add records an entry, replacing any earlier one at the same path so that a
// generated file (a mailbox, a history) can be overridden by an authored one.
func (r *rootfs) add(e entry) {
	e.Path = path.Clean(e.Path)
	if at, ok := r.index[e.Path]; ok {
		r.entries[at] = e
		return
	}
	r.index[e.Path] = len(r.entries)
	r.entries = append(r.entries, e)
}

// has reports whether a path has already been laid down.
func (r *rootfs) has(p string) bool {
	_, ok := r.index[path.Clean(p)]
	return ok
}

// dirs materialises the parent directories of every entry, so that a file at
// ~/.local/share/notes.txt does not depend on its ancestors having been
// declared. Directories inherit the owner and time of the deepest file under
// them, which is what a directory's mtime means anyway: when its contents last
// changed.
func (r *rootfs) dirs() []entry {
	needed := map[string]entry{}

	for _, e := range r.entries {
		for dir := path.Dir(e.Path); dir != "/" && dir != "."; dir = path.Dir(dir) {
			if r.has(dir) {
				break
			}
			prior, seen := needed[dir]
			if !seen || e.ModTime.After(prior.ModTime) {
				needed[dir] = entry{
					Path: dir, Dir: true, Mode: 0o755,
					UID: e.UID, GID: e.GID, ModTime: e.ModTime,
				}
			}
		}
	}

	out := make([]entry, 0, len(needed))
	for _, e := range needed {
		out = append(out, e)
	}
	return out
}

// all returns every entry including synthesised directories, ordered so that a
// directory always precedes what it contains.
func (r *rootfs) all() []entry {
	all := append(append([]entry{}, r.dirs()...), r.entries...)

	// Ancestors of an authored path, and the system directories an authored
	// path happens to sit in, keep whatever owns them in the base image.
	for i := range all {
		if isSystemPath(all[i].Path) {
			all[i].UID, all[i].GID = -1, -1
		}
	}

	sort.Slice(all, func(i, j int) bool {
		if depth(all[i].Path) != depth(all[j].Path) {
			return depth(all[i].Path) < depth(all[j].Path)
		}
		return all[i].Path < all[j].Path
	})
	return all
}

func depth(p string) int { return strings.Count(path.Clean(p), "/") }

// systemTrees are never handed to a level, nor anything beneath them.
var systemTrees = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32",
	"/boot", "/proc", "/sys", "/dev", "/run",
}

// systemDirs are never handed to a level, though a level may own something
// placed inside them.
var systemDirs = map[string]bool{
	"/": true, "/etc": true, "/home": true, "/media": true, "/mnt": true,
	"/opt": true, "/root": true, "/srv": true, "/tmp": true, "/var": true,
	"/var/log": true, "/var/lib": true, "/var/cache": true, "/var/spool": true,
	"/var/tmp": true, "/var/mail": true, "/var/opt": true, "/var/local": true,
}

// isSystemPath reports whether a path belongs to the operating system rather
// than to any level.
//
// It exists because placing a file under files/var/backups/ means the author
// wants that file, and plausibly that directory -- but not /var. Without this,
// every ancestor of an authored path is chowned to the level, and the box ends
// up with a mail operator owning /var, which is both absurd on its face and a
// permission model nobody intended.
func isSystemPath(p string) bool {
	p = path.Clean(p)
	if systemDirs[p] {
		return true
	}
	for _, tree := range systemTrees {
		if p == tree || strings.HasPrefix(p, tree+"/") {
			return true
		}
	}
	return false
}

// writeTar writes the rootfs into a tar under the given prefix.
//
// Ownership and timestamps travel in the tar headers rather than being applied
// by a script afterwards. Docker's COPY preserves both, so the image gets the
// intended metadata without the game image needing any tooling of its own.
func (r *rootfs) writeTar(tw *tar.Writer, prefix string) error {
	for _, e := range r.all() {
		if e.MetaOnly {
			continue
		}
		name := path.Join(prefix, strings.TrimPrefix(e.Path, "/"))

		header := &tar.Header{
			Name:    name,
			Mode:    int64(e.Mode.Perm()),
			Uid:     e.UID,
			Gid:     e.GID,
			ModTime: e.ModTime,
			Format:  tar.FormatPAX,
		}

		switch {
		case e.Dir:
			header.Typeflag = tar.TypeDir
			header.Name = name + "/"
		case e.Link != "":
			header.Typeflag = tar.TypeSymlink
			header.Linkname = e.Link
		default:
			header.Typeflag = tar.TypeReg
			header.Size = int64(len(e.Content))
		}

		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("write header for %s: %w", e.Path, err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := tw.Write(e.Content); err != nil {
				return fmt.Errorf("write %s: %w", e.Path, err)
			}
		}
	}
	return nil
}

// metaPlan renders the ownership, permission and timestamp fix-up applied as
// the last step of the build.
//
// It is not belt-and-braces; it is load-bearing twice over. Docker's COPY
// discards the ownership in a tar header and makes everything root-owned, so
// the plan is the only thing that gives a level's files to its account. And
// every RUN after the COPY -- a setup script fixing permissions, a package
// hook, useradd writing /etc/passwd -- re-stamps whatever it touches with the
// build time, which is the exact tell the aging exists to remove.
//
// Each line is: owner, mtime, path. A dash for the owner leaves it alone.
//
// Permissions are deliberately absent. COPY preserves the mode in a tar header
// (it is only ownership it discards), so the author's mode on disk already
// arrives intact -- and a level's setup.sh must have the last word on
// permissions, since that is where the game's access control is expressed.
func (r *rootfs) metaPlan() []byte {
	all := r.all()

	// Files first. Creating or removing an entry updates its directory's mtime,
	// so directories are stamped afterwards, deepest first, and keep the time
	// they were meant to have.
	var files, directories []entry
	for _, e := range all {
		if e.Dir {
			directories = append(directories, e)
		} else {
			files = append(files, e)
		}
	}
	sort.Slice(directories, func(i, j int) bool {
		return depth(directories[i].Path) > depth(directories[j].Path)
	})

	var b bytes.Buffer
	for _, e := range append(files, directories...) {
		owner := "-:-"
		if e.UID >= 0 {
			owner = fmt.Sprintf("%d:%d", e.UID, e.GID)
		}
		fmt.Fprintf(&b, "%s\t%d\t%s\n", owner, e.ModTime.Unix(), e.Path)
	}
	return b.Bytes()
}

// addMeta records a timestamp (and optionally ownership) for a file the build
// creates itself, without shipping any content for it.
func (r *rootfs) addMeta(p string, at time.Time) {
	if r.has(p) {
		return
	}
	r.add(entry{Path: p, UID: -1, GID: -1, ModTime: at, MetaOnly: true})
}

// addFile is the common case: a regular file owned by an account.
func (r *rootfs) addFile(p string, content []byte, mode fs.FileMode, owner *account, at time.Time) {
	r.add(entry{
		Path: p, Content: content, Mode: mode,
		UID: owner.UID, GID: owner.GID, ModTime: at,
	})
}
