package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/klahr/wge/internal/docker"
	"github.com/klahr/wge/internal/runtime"
)

// cmdBase builds one of the base images game images are built on. Bases change
// rarely and are shared by every game, so they are built separately rather than
// rebuilt with each game.
func cmdBase(args []string) error {
	fs := flag.NewFlagSet("base", flag.ExitOnError)
	socket := fs.String("docker", runtime.DefaultSocket, "Docker engine socket")
	tag := fs.String("tag", "", "image tag (default: wge/<directory name>)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := applyEnv(fs); err != nil {
		return err
	}
	dir := fs.Arg(0)
	if dir == "" {
		return fmt.Errorf("usage: wge base [-tag name] <base-dir>")
	}

	name := *tag
	if name == "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		name = "wge/" + filepath.Base(abs)
	}

	archive, err := tarDir(dir)
	if err != nil {
		return fmt.Errorf("assemble build context: %w", err)
	}

	if err := docker.New(*socket).Build(context.Background(), name, archive, os.Stderr); err != nil {
		return err
	}
	fmt.Printf("\n%s\n", name)
	return nil
}
