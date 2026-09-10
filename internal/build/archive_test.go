package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// packArchive builds the whole file rather than repacking one, which is what
// makes a credential inside an archive possible: seeding cannot edit a member
// in place, but it can produce the archive again.
func TestPackedArchiveIsReadableAndAged(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"home/d/.ssh/id_ed25519", "home/d/.ssh/known_hosts", "home/d/notes.txt"} {
		full := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("contents of "+f), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	aging := testAging()
	blob, err := packArchive(dir, "/var/backups/nightly.tar.gz", aging,
		Owner{User: "bkup"}, 1201, 1201, nil)
	if err != nil {
		t.Fatalf("packArchive: %v", err)
	}

	members := readArchive(t, blob)
	for _, want := range []string{"home/d/.ssh/id_ed25519", "home/d/notes.txt"} {
		if _, ok := members[want]; !ok {
			t.Fatalf("%s is missing from the archive; got %v", want, keys(members))
		}
	}

	// A tarball whose members are all dated to the build is a tell the moment
	// somebody extracts it, and extracting it is the point.
	seen := map[int64]string{}
	for name, h := range members {
		at := h.ModTime.Unix()
		if prior, dup := seen[at]; dup {
			t.Errorf("%s and %s share a timestamp inside the archive", prior, name)
		}
		seen[at] = name
		if h.ModTime.After(aging.Now()) {
			t.Errorf("%s is dated %v, after the box's present", name, h.ModTime)
		}
	}

	if got := members["home/d/.ssh/id_ed25519"].Uid; got != 1201 {
		t.Errorf("member uid = %d, want the staging account's", got)
	}
}

// The rendering hook is what puts a per-run credential inside.
func TestPackedArchiveRendersItsMembers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "key"), []byte("{{ secret }}"), 0o600); err != nil {
		t.Fatal(err)
	}

	blob, err := packArchive(dir, "/var/backups/nightly.tar.gz", testAging(),
		Owner{User: "bkup"}, 0, 0,
		func(member string, content []byte) ([]byte, error) {
			if !bytes.Contains(content, []byte("{{")) {
				return content, nil
			}
			return []byte("rendered for " + member), nil
		})
	if err != nil {
		t.Fatal(err)
	}

	members := readArchive(t, blob)
	got := string(members["key"].body)
	if got != "rendered for /var/backups/nightly.tar.gz:key" {
		t.Fatalf("member content = %q", got)
	}
}

func TestArchiveDirectoriesAreRecognised(t *testing.T) {
	if !isArchiveDir("nightly.tar.gz") {
		t.Error("a .tar.gz directory should be packed")
	}
	if isArchiveDir("restore-2024-11-02") {
		t.Error("an ordinary directory should not be packed")
	}
}

// Rebuilding an archive on every container start is wasted work when nothing
// in it needs rendering.
func TestArchivesWithoutTemplatesAreLeftAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plain"), []byte("no credentials here"), 0o600); err != nil {
		t.Fatal(err)
	}

	templated, err := archiveContainsTemplate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if templated {
		t.Fatal("an archive with no templates was reported as needing rendering")
	}

	if err := os.WriteFile(filepath.Join(dir, "key"), []byte(`{{ .Password "x" }}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if templated, err = archiveContainsTemplate(dir); err != nil || !templated {
		t.Fatalf("an archive holding a template was not noticed (%v, %v)", templated, err)
	}
}

type member struct {
	*tar.Header
	body []byte
}

func readArchive(t *testing.T, blob []byte) map[string]member {
	t.Helper()

	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("the archive is not gzipped: %v", err)
	}
	tr := tar.NewReader(zr)

	out := map[string]member{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("the archive is not a tar: %v", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = member{Header: h, body: body}
	}
	return out
}

func keys(m map[string]member) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
