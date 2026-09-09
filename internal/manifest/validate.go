package manifest

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Errors is a collection of manifest problems. Authors get every fault in one
// pass rather than peeling them off one build at a time.
type Errors []error

func (e Errors) Error() string {
	if len(e) == 1 {
		return e[0].Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d problems:", len(e))
	for _, err := range e {
		fmt.Fprintf(&b, "\n  - %s", err)
	}
	return b.String()
}

func (e Errors) Unwrap() []error { return e }

// Validate checks everything about a game that can be known without building
// it: identifier hygiene, the shape of the level graph, and -- most
// importantly -- that the graph and the credential placements agree with each
// other.
//
// The checks that need a running container (can this user actually read that
// file, does anything leak upward) live in the build-time test pass; this is
// the half that should fail in an author's editor.
func (g *Game) Validate() error {
	var errs Errors

	errs = append(errs, g.validateMeta()...)
	errs = append(errs, g.validateHosts()...)
	errs = append(errs, g.validateLevels()...)
	errs = append(errs, g.validateGraph()...)
	errs = append(errs, g.validateServices()...)

	if len(errs) == 0 {
		return nil
	}
	return errs
}

func (g *Game) validateMeta() Errors {
	var errs Errors
	if !slugPattern.MatchString(g.ID) {
		errs = append(errs, fmt.Errorf("game id %q must be a lowercase slug", g.ID))
	}
	if g.Version < 1 {
		errs = append(errs, fmt.Errorf("game version must be 1 or greater, got %d", g.Version))
	}
	if strings.TrimSpace(g.Title) == "" {
		errs = append(errs, fmt.Errorf("game title is required"))
	}
	if len(g.Hosts) == 0 && strings.TrimSpace(g.Base) == "" {
		errs = append(errs, fmt.Errorf("game must set base, or declare hosts that do"))
	}
	// Without a fictional clock there is nothing to age files against, and
	// every mtime in the image collapses onto the build timestamp.
	if g.Timeline.Start.IsZero() {
		errs = append(errs, fmt.Errorf("timeline.start is required so file timestamps can be aged"))
	}
	if g.NoiseUsers < 0 {
		errs = append(errs, fmt.Errorf("noise_users cannot be negative"))
	}
	if g.Timeline.Span < 0 {
		errs = append(errs, fmt.Errorf("timeline.span cannot be negative"))
	}
	return errs
}

func (g *Game) validateHosts() Errors {
	var errs Errors
	seen := map[string]bool{}
	for _, h := range g.Hosts {
		if !slugPattern.MatchString(h.ID) {
			errs = append(errs, fmt.Errorf("host id %q must be a lowercase slug", h.ID))
			continue
		}
		if seen[h.ID] {
			errs = append(errs, fmt.Errorf("host %q is declared twice", h.ID))
		}
		seen[h.ID] = true
		if strings.TrimSpace(h.Base) == "" && strings.TrimSpace(g.Base) == "" {
			errs = append(errs, fmt.Errorf("host %q has no base image and the game sets none", h.ID))
		}
	}
	for _, h := range g.Hosts {
		for _, peer := range h.Egress {
			switch {
			case peer == h.ID:
				errs = append(errs, fmt.Errorf("host %q lists itself in egress", h.ID))
			case !seen[peer]:
				errs = append(errs, fmt.Errorf("host %q allows egress to unknown host %q", h.ID, peer))
			}
		}
	}
	return errs
}

func (g *Game) validateLevels() Errors {
	var errs Errors
	hosts := map[string]bool{}
	for _, id := range g.HostIDs() {
		hosts[id] = true
	}

	ids := map[string]bool{}
	users := map[string]string{} // user -> level that claimed it

	for _, l := range g.Levels {
		where := l.ID
		if where == "" {
			where = path.Base(l.Dir)
		}

		if !slugPattern.MatchString(l.ID) {
			errs = append(errs, fmt.Errorf("level %s: id must be a lowercase slug", where))
		} else if ids[l.ID] {
			errs = append(errs, fmt.Errorf("level %q is declared twice", l.ID))
		}
		ids[l.ID] = true

		if !userPattern.MatchString(l.User) || len(l.User) > 32 {
			errs = append(errs, fmt.Errorf("level %s: %q is not a valid Unix username", where, l.User))
		} else if prior, dup := users[l.User]; dup {
			// Two levels sharing a uid is the same level twice: solving one
			// silently solves the other.
			errs = append(errs, fmt.Errorf("level %s and level %s both use the account %q", prior, where, l.User))
		} else {
			users[l.User] = where
		}

		if !hosts[l.Host] {
			errs = append(errs, fmt.Errorf("level %s: unknown host %q", where, l.Host))
		}

		errs = append(errs, l.validateGrants()...)
	}
	return errs
}

func (l *Level) validateGrants() Errors {
	var errs Errors
	for i, gr := range l.Grants {
		switch gr.Kind {
		case GrantPassword, GrantSSHKey, GrantPassphrase:
		default:
			errs = append(errs, fmt.Errorf("level %s: grant %d has unknown kind %q", l.ID, i, gr.Kind))
		}
		if gr.To == l.ID {
			errs = append(errs, fmt.Errorf("level %s: grants a credential to itself", l.ID))
		}
		if strings.TrimSpace(gr.PlacedIn) == "" {
			errs = append(errs, fmt.Errorf("level %s: grant to %q does not say where the credential is placed", l.ID, gr.To))
			continue
		}
		if !path.IsAbs(gr.Path()) {
			errs = append(errs, fmt.Errorf("level %s: grant to %q has a relative path %q", l.ID, gr.To, gr.Path()))
		}
		// Rendering a per-run credential into a member of an archive means
		// repacking the archive during seeding, which the pipeline does not do
		// yet. Accepting it here would produce a game whose credential never
		// appears.
		if gr.Member() != "" {
			errs = append(errs, fmt.Errorf(
				"level %s: grant to %q is placed inside the archive %q; credentials inside archives are not supported yet, place it in a plain file",
				l.ID, gr.To, gr.Path()))
		}
	}
	return errs
}

// validateGraph checks the level graph itself, and the agreement between the
// graph and the credential placements. A level that declares a prerequisite the
// prerequisite does not actually grant is the most common way an authored game
// becomes unsolvable, and it is entirely mechanical to catch.
func (g *Game) validateGraph() Errors {
	var errs Errors

	known := map[string]bool{}
	for _, l := range g.Levels {
		known[l.ID] = true
	}

	// Prerequisites must exist and be distinct.
	for _, l := range g.Levels {
		seen := map[string]bool{}
		for _, req := range l.Requires {
			switch {
			case req == l.ID:
				errs = append(errs, fmt.Errorf("level %s: requires itself", l.ID))
			case !known[req]:
				errs = append(errs, fmt.Errorf("level %s: requires unknown level %q", l.ID, req))
			case seen[req]:
				errs = append(errs, fmt.Errorf("level %s: requires %q twice", l.ID, req))
			}
			seen[req] = true
		}
		for _, gr := range l.Grants {
			if gr.To != "" && !known[gr.To] {
				errs = append(errs, fmt.Errorf("level %s: grants to unknown level %q", l.ID, gr.To))
			}
		}
	}

	// Exactly one entry: the player needs one place to start, and the run's
	// starting password has to name a single level.
	var entries []string
	for _, l := range g.Levels {
		if len(l.Requires) == 0 {
			entries = append(entries, l.ID)
		}
	}
	switch len(entries) {
	case 1:
	case 0:
		errs = append(errs, fmt.Errorf("no entry level: every level has prerequisites, so the graph is a cycle"))
	default:
		sort.Strings(entries)
		errs = append(errs, fmt.Errorf("levels %s all have no prerequisites; exactly one entry level is allowed", strings.Join(entries, ", ")))
	}

	errs = append(errs, g.validateGrantAgreement(known)...)
	errs = append(errs, g.validateAcyclic()...)
	errs = append(errs, g.validateReachable(entries)...)

	// A game needs somewhere to end. Every level granting onward means the
	// player never arrives anywhere.
	terminal := false
	for _, l := range g.Levels {
		if len(l.Grants) == 0 {
			terminal = true
			break
		}
	}
	if !terminal && len(g.Levels) > 0 {
		errs = append(errs, fmt.Errorf("no final level: every level grants a credential onward"))
	}

	return errs
}

// validateGrantAgreement is the check that matters most: the edges asserted by
// requires and the edges implied by grants must be the same set of edges, and a
// level with several prerequisites must receive a distinct kind of credential
// from each of them.
func (g *Game) validateGrantAgreement(known map[string]bool) Errors {
	var errs Errors

	// incoming[target][source] = kinds granted along that edge
	incoming := map[string]map[string][]GrantKind{}
	for _, l := range g.Levels {
		for _, gr := range l.Grants {
			if !known[gr.To] {
				continue // already reported
			}
			if incoming[gr.To] == nil {
				incoming[gr.To] = map[string][]GrantKind{}
			}
			incoming[gr.To][l.ID] = append(incoming[gr.To][l.ID], gr.Kind)
		}
	}

	for _, l := range g.Levels {
		from := incoming[l.ID]

		// Every prerequisite must actually hand over a credential.
		for _, req := range l.Requires {
			if !known[req] {
				continue
			}
			if len(from[req]) == 0 {
				errs = append(errs, fmt.Errorf("level %s requires %s, but %s grants it nothing: the level is unreachable", l.ID, req, req))
			}
		}

		// And every credential handed over must be declared as a prerequisite,
		// or the graph understates what a player needs to have solved.
		declared := map[string]bool{}
		for _, req := range l.Requires {
			declared[req] = true
		}
		for src := range from {
			if !declared[src] {
				errs = append(errs, fmt.Errorf("level %s grants a credential to %s, but %s does not list it as a prerequisite", src, l.ID, l.ID))
			}
		}

		errs = append(errs, l.validateCredentialSet(from)...)
	}
	return errs
}

// validateCredentialSet checks that what a level receives adds up to a way in.
func (l *Level) validateCredentialSet(from map[string][]GrantKind) Errors {
	var errs Errors
	if len(from) == 0 {
		return nil // entry level, or already reported as unreachable
	}

	kinds := map[GrantKind]string{}
	for src, granted := range from {
		for _, k := range granted {
			if prior, dup := kinds[k]; dup {
				// Two levels handing over the same credential means either is
				// sufficient, so the second prerequisite is not really required.
				errs = append(errs, fmt.Errorf("level %s receives a %s from both %s and %s; one of them is not actually needed", l.ID, k, prior, src))
			}
			kinds[k] = src
		}
	}

	_, hasPassword := kinds[GrantPassword]
	_, hasKey := kinds[GrantSSHKey]
	_, hasPassphrase := kinds[GrantPassphrase]

	if hasPassphrase && !hasKey {
		errs = append(errs, fmt.Errorf("level %s receives a key passphrase but no key: the passphrase opens nothing", l.ID))
	}
	if !hasPassword && !hasKey {
		errs = append(errs, fmt.Errorf("level %s receives no password and no key: there is no way in", l.ID))
	}
	return errs
}

func (g *Game) validateAcyclic() Errors {
	const (
		unvisited = 0
		active    = 1
		done      = 2
	)
	state := map[string]int{}
	var stack []string
	var errs Errors

	var walk func(id string) bool
	walk = func(id string) bool {
		l, ok := g.Level(id)
		if !ok {
			return false
		}
		switch state[id] {
		case done:
			return false
		case active:
			// Report the cycle itself; "there is a cycle" is not actionable.
			at := len(stack) - 1
			for at >= 0 && stack[at] != id {
				at--
			}
			cycle := append(append([]string{}, stack[max(at, 0):]...), id)
			errs = append(errs, fmt.Errorf("prerequisite cycle: %s", strings.Join(cycle, " -> ")))
			return true
		}

		state[id] = active
		stack = append(stack, id)
		for _, req := range l.Requires {
			if walk(req) {
				break // one report per cycle is enough
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = done
		return false
	}

	for _, l := range g.Levels {
		walk(l.ID)
	}
	return errs
}

func (g *Game) validateReachable(entries []string) Errors {
	if len(entries) != 1 {
		return nil // reachability is meaningless until there is one entry
	}

	// Walk forward along grants from the entry level.
	reached := map[string]bool{entries[0]: true}
	queue := []string{entries[0]}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		l, ok := g.Level(id)
		if !ok {
			continue
		}
		for _, gr := range l.Grants {
			if !reached[gr.To] {
				reached[gr.To] = true
				queue = append(queue, gr.To)
			}
		}
	}

	var orphans []string
	for _, l := range g.Levels {
		if !reached[l.ID] {
			orphans = append(orphans, l.ID)
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	sort.Strings(orphans)
	return Errors{fmt.Errorf("levels unreachable from %s: %s", entries[0], strings.Join(orphans, ", "))}
}

func (g *Game) validateServices() Errors {
	var errs Errors

	levelUsers := map[string]string{}
	for _, l := range g.Levels {
		levelUsers[l.User] = l.ID
	}

	seen := map[string]string{}
	for _, l := range g.Levels {
		for _, s := range l.Services {
			if strings.TrimSpace(s.Name) == "" {
				errs = append(errs, fmt.Errorf("level %s: a service has no name", l.ID))
				continue
			}
			if !userPattern.MatchString(s.User) {
				errs = append(errs, fmt.Errorf("level %s: service %q has invalid user %q", l.ID, s.Name, s.User))
			}
			// A service running as root hands root to whoever compromises it,
			// which collapses every level in the game at once.
			if s.User == "root" {
				errs = append(errs, fmt.Errorf("level %s: service %q runs as root; give it a dedicated unprivileged account", l.ID, s.Name))
			}
			// Sharing a uid with a level makes compromising the service
			// equivalent to solving that level, without the graph saying so.
			if owner, ok := levelUsers[s.User]; ok {
				errs = append(errs, fmt.Errorf("level %s: service %q runs as %q, which is level %s's account", l.ID, s.Name, s.User, owner))
			}
			if prior, dup := seen[s.Name]; dup {
				errs = append(errs, fmt.Errorf("service %q is declared by both level %s and level %s", s.Name, prior, l.ID))
			}
			seen[s.Name] = l.ID
			if s.Port < 0 || s.Port > 65535 {
				errs = append(errs, fmt.Errorf("level %s: service %q has port %d out of range", l.ID, s.Name, s.Port))
			}
		}
	}
	return errs
}
