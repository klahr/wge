// Command wge is the engine's control surface: validating and building games,
// and running the SSH front door that players connect to.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/klahr/wge/internal/creds"
	"github.com/klahr/wge/internal/manifest"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "wge: %v\n", err)
		os.Exit(1)
	}
}

const usage = `wge -- an engine for hacking simulators played over SSH

usage: wge <command> [arguments]

commands:
  validate <game-dir>    check a game manifest and its level graph
  graph    <game-dir>    print the level graph and the credentials along it
  creds    <game-dir>    derive the credentials for one run (support tool)
  base     <base-dir>    build a base image games are built on
  build    <game-dir>    compile a game into a container image
  test     <game-dir>    verify a built image enforces its level graph
  serve                  run the SSH front door
`

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("no command given")
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "validate":
		return cmdValidate(rest)
	case "graph":
		return cmdGraph(rest)
	case "creds":
		return cmdCreds(rest)
	case "base":
		return cmdBase(rest)
	case "build":
		return cmdBuild(rest)
	case "test":
		return cmdTest(rest)
	case "serve":
		return cmdServe(rest)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := fs.Arg(0)
	if dir == "" {
		return fmt.Errorf("usage: wge validate <game-dir>")
	}

	g, err := manifest.Load(dir)
	if err != nil {
		return err
	}

	entry, _ := g.Entry()
	fmt.Printf("%s v%d -- %s\n", g.ID, g.Version, g.Title)
	fmt.Printf("  %d levels across %d host(s), entry level %s\n",
		len(g.Levels), len(g.HostIDs()), entry.ID)
	fmt.Printf("  %d noise users\n", g.NoiseUsers)
	fmt.Println("ok")
	return nil
}

func cmdGraph(args []string) error {
	fs := flag.NewFlagSet("graph", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := fs.Arg(0)
	if dir == "" {
		return fmt.Errorf("usage: wge graph <game-dir>")
	}

	g, err := manifest.Load(dir)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LEVEL\tUSER\tHOST\tREQUIRES\tGRANTS")
	for _, l := range g.Levels {
		var grants []string
		for _, gr := range l.Grants {
			grants = append(grants, fmt.Sprintf("%s->%s", gr.Kind, gr.To))
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			l.ID, l.User, l.Host,
			orDash(strings.Join(l.Requires, ",")),
			orDash(strings.Join(grants, " ")))
	}
	return w.Flush()
}

// cmdCreds re-derives a run's credentials. Because secrets are a pure function
// of the salt, a player who has lost their notes can be helped without anyone
// having stored a password anywhere.
func cmdCreds(args []string) error {
	fs := flag.NewFlagSet("creds", flag.ExitOnError)
	salt := fs.String("salt", "", "run salt (hex); a random one is used if empty")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := fs.Arg(0)
	if dir == "" {
		return fmt.Errorf("usage: wge creds [-salt hex] <game-dir>")
	}

	g, err := manifest.Load(dir)
	if err != nil {
		return err
	}

	var s creds.Salt
	if *salt == "" {
		if s, err = creds.NewSalt(); err != nil {
			return err
		}
	} else if s, err = creds.ParseSalt(*salt); err != nil {
		return err
	}

	d := creds.New(s, g.ID)
	fmt.Printf("run salt: %s\n\n", s)

	ids := g.LevelIDs()
	sort.Strings(ids)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LEVEL\tPASSWORD\tKEY PASSPHRASE")
	for _, id := range ids {
		fmt.Fprintf(w, "%s\t%s\t%s\n", id, d.Password(id), d.Passphrase(id))
	}
	return w.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
