package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
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
