package main

import (
	"archive/tar"
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// tarDir packs a directory as a Docker build context.
func tarDir(dir string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err := filepath.WalkDir(dir, func(src string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, src)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		content, err := os.ReadFile(src)
		if err != nil {
			return err
		}

		if err := tw.WriteHeader(&tar.Header{
			Name: filepath.ToSlash(rel), Mode: int64(info.Mode().Perm()),
			Size: int64(len(content)), ModTime: info.ModTime(),
			Typeflag: tar.TypeReg, Format: tar.FormatPAX,
		}); err != nil {
			return err
		}
		_, err = tw.Write(content)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}
