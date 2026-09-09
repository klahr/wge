package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/klahr/wge/internal/build"
	"github.com/klahr/wge/internal/library"
	"github.com/klahr/wge/internal/manifest"
	"github.com/klahr/wge/internal/runtime"
)

func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	socket := fs.String("docker", runtime.DefaultSocket, "Docker engine socket")
	quiet := fs.Bool("q", false, "suppress build output")
	host := fs.String("host", "", "build only this host (default: all of them)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := fs.Arg(0)
	if dir == "" {
		return fmt.Errorf("usage: wge build [-host id] <game-dir>")
	}

	g, err := manifest.Load(dir)
	if err != nil {
		return err
	}

	var progress io.Writer = os.Stderr
	if *quiet {
		progress = nil
	}

	builder := build.NewBuilder(*socket)
	ctx := context.Background()

	for _, h := range g.HostIDs() {
		if *host != "" && h != *host {
			continue
		}

		tag := library.ImageName(g.ID, g.Version, h)
		result, err := builder.Build(ctx, g, h, tag, progress)
		if err != nil {
			return fmt.Errorf("build %s: %w", tag, err)
		}

		fmt.Printf("\n%s\n", result.Image)
		fmt.Printf("  %d accounts, %d files\n", result.Accounts, result.Files)
		fmt.Printf("  timestamps span %s .. %s\n",
			result.Oldest.Format("2006-01-02"), result.Newest.Format("2006-01-02"))
	}
	return nil
}
