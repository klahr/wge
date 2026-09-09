package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/klahr/wge/internal/build"
	"github.com/klahr/wge/internal/library"
	"github.com/klahr/wge/internal/manifest"
	"github.com/klahr/wge/internal/runtime"
)

// cmdTest verifies a built image from inside a running container.
//
// The static validator answers "does this manifest describe a solvable game".
// This answers "does the filesystem the build produced actually enforce it",
// which is a different question and the one authored games fail.
func cmdTest(args []string) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	socket := fs.String("docker", runtime.DefaultSocket, "Docker engine socket")
	host := fs.String("host", "", "test only this host (default: all of them)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := fs.Arg(0)
	if dir == "" {
		return fmt.Errorf("usage: wge test [-host id] <game-dir>")
	}

	g, err := manifest.Load(dir)
	if err != nil {
		return err
	}

	verifier := build.NewVerifier(*socket)
	ctx := context.Background()
	failed := false

	for _, h := range g.HostIDs() {
		if *host != "" && h != *host {
			continue
		}

		image := library.ImageName(g.ID, g.Version, h)
		fmt.Printf("verifying %s\n", image)

		report, err := verifier.Verify(ctx, g, h, image)
		if err != nil {
			return err
		}

		for _, f := range report.Findings {
			fmt.Fprintf(os.Stderr, "  %s\n", f)
		}
		fmt.Printf("  %d checks across %d levels", report.Checks, report.Levels)
		if report.OK() {
			fmt.Printf(" -- ok\n")
			continue
		}
		fmt.Printf(" -- %d problems\n", len(report.Findings))
		failed = true
	}

	if failed {
		return fmt.Errorf("the image does not enforce the level graph")
	}
	return nil
}
