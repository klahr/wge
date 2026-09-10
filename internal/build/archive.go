package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ArchiveSuffix marks a directory in a level's files/ tree that becomes an
// archive rather than a directory.
//
// files/var/backups/nightly.tar.gz/ is written into the image as the file
// /var/backups/nightly.tar.gz, packed from what is inside it. A tarball of
// somebody's home directory is much better fiction than the extracted copy
// standing in for one, and it is the shape a real staged backup has.
const ArchiveSuffix = ".tar.gz"

// isArchiveDir reports whether a directory should be packed rather than walked.
func isArchiveDir(name string) bool { return strings.HasSuffix(name, ArchiveSuffix) }

// archiveMember renders one file's content on the way into an archive.
//
// At build time it is the identity: the archive carries the unrendered
// template, which nobody sees because seeding replaces the whole file before a
// player can reach it. At seed time it is the template renderer.
type archiveMember func(memberPath string, content []byte) ([]byte, error)

// packArchive builds a gzipped tar from a directory.
//
// The archive is built whole rather than repacked, which is what makes a
// credential inside one possible at all: seeding cannot edit a member in
// place, but it can produce the entire file again and upload it.
//
// Member timestamps are aged like everything else. A tarball whose contents
// are all dated to the build is a tell the moment somebody extracts it, and
// extracting it is the point.
func packArchive(root, archivePath string, aging *Aging, owner Owner, uid, gid int, render archiveMember) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	// The archive's own stored name and time, as tar records them.
	zw.Name = path.Base(archivePath)
	zw.ModTime = aging.FileTime(owner, archivePath)

	tw := tar.NewWriter(zw)

	err := filepath.WalkDir(root, func(src string, d fs.DirEntry, err error) error {
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

		member := filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}

		// Members are aged against their path inside the archive, so two
		// archives holding the same name do not share a timestamp.
		at := aging.FileTime(owner, archivePath+":"+member)

		header := &tar.Header{
			Name: member, Mode: int64(info.Mode().Perm()),
			Uid: uid, Gid: gid, ModTime: at, Format: tar.FormatPAX,
		}

		switch {
		case d.IsDir():
			header.Typeflag = tar.TypeDir
			header.Name = member + "/"
			return tw.WriteHeader(header)
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(src)
			if err != nil {
				return err
			}
			header.Typeflag = tar.TypeSymlink
			header.Linkname = target
			return tw.WriteHeader(header)
		}

		content, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if render != nil {
			if content, err = render(archivePath+":"+member, content); err != nil {
				return err
			}
		}

		header.Typeflag = tar.TypeReg
		header.Size = int64(len(content))
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		_, err = tw.Write(content)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("pack %s: %w", archivePath, err)
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// archiveContainsTemplate reports whether anything in the directory needs
// rendering, so an archive with no credentials in it is not rebuilt on every
// container start.
func archiveContainsTemplate(root string) (bool, error) {
	var found bool
	err := filepath.WalkDir(root, func(src string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found {
			return err
		}
		raw, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if bytes.Contains(raw, []byte(templateMarker)) {
			found = true
		}
		return nil
	})
	return found, err
}
