package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/klahr/wge/internal/store"
)

// envPrefix is what a flag's environment equivalent is called.
const envPrefix = "WGE_"

// applyEnv fills in the flags that were not given on the command line from the
// environment: -max-machines is WGE_MAX_MACHINES.
//
// It exists for the unit file. A service that has to carry a dozen flags in its
// ExecStart is a service whose configuration is not reviewable, cannot be
// commented, and changes only by editing a unit -- whereas an EnvironmentFile
// is a plain list of settings an operator can read and annotate.
//
// The command line wins where both are given, so an operator can always
// override the file for one run without editing it.
func applyEnv(fs *flag.FlagSet) error {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	var failed error
	fs.VisitAll(func(f *flag.Flag) {
		if failed != nil || given[f.Name] {
			return
		}
		value, ok := os.LookupEnv(envName(f.Name))
		if !ok {
			return
		}
		if err := fs.Set(f.Name, value); err != nil {
			failed = fmt.Errorf("%s=%q: %w", envName(f.Name), value, err)
		}
	})
	return failed
}

func envName(flagName string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// openStore opens the engine database for a command that reads one, refusing
// to create it.
//
// serve makes the database; every other command works on the database serve
// made. The default path is relative, so a command run from the wrong
// directory would otherwise open a new, empty one and succeed -- which is an
// invitation no server will ever accept, or a reset of a player who is not
// there. The path is reported absolute, because the mistake is a directory.
func openStore(ctx context.Context, path string) (*store.Store, error) {
	if _, err := os.Stat(path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		shown := path
		if abs, err := filepath.Abs(path); err == nil {
			shown = abs
		}
		return nil, fmt.Errorf("no engine database at %s; wge serve creates it, "+
			"so run this where the server runs or pass -db", shown)
	}
	return store.Open(ctx, path)
}
