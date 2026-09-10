// Package library loads the games an engine instance serves and names the
// images that back them.
package library

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klahr/wge/internal/manifest"
)

// Library is a set of loaded, validated games.
type Library struct {
	games map[string]*manifest.Game
}

// Load reads every game directory under root. A game that fails validation
// fails the load: serving a game the validator rejects means serving one that
// may not be solvable.
func Load(root string) (*Library, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read game library: %w", err)
	}

	lib := &Library{games: map[string]*manifest.Game{}}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, manifest.GameFile)); os.IsNotExist(err) {
			continue
		}

		g, err := manifest.Load(dir)
		if err != nil {
			return nil, fmt.Errorf("game %s: %w", e.Name(), err)
		}
		if prior, dup := lib.games[g.ID]; dup {
			return nil, fmt.Errorf("game id %q is claimed by both %s and %s", g.ID, prior.Dir, dir)
		}
		lib.games[g.ID] = g
	}

	if len(lib.games) == 0 {
		return nil, fmt.Errorf("no games found in %s", root)
	}
	return lib, nil
}

// Only narrows the library to the games named, for an engine that serves one
// game out of a directory that holds several. Images are only required for the
// games served, so serving one game does not mean building the rest.
//
// An id that names no game is an error rather than an empty selection: the
// alternative turns a typo in the name of the only game being served into a
// server that starts, offers nothing, and answers no password anybody has.
func (l *Library) Only(ids []string) (*Library, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("no game named to serve")
	}

	out := &Library{games: map[string]*manifest.Game{}}
	for _, id := range ids {
		g, ok := l.games[id]
		if !ok {
			return nil, fmt.Errorf("no game %q in the library; it holds: %s",
				id, strings.Join(l.IDs(), ", "))
		}
		out.games[g.ID] = g
	}
	return out, nil
}

// IDs returns the id of every game, ordered.
func (l *Library) IDs() []string {
	out := make([]string, 0, len(l.games))
	for id := range l.games {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Game implements broker.Games.
func (l *Library) Game(id string) (*manifest.Game, bool) {
	g, ok := l.games[id]
	return g, ok
}

// All returns every game, ordered by id.
func (l *Library) All() []*manifest.Game {
	out := make([]*manifest.Game, 0, len(l.games))
	for _, g := range l.games {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Image implements runtime.Images.
//
// The version in the tag is the run's pinned version, not the library's: a
// player halfway through v3 keeps getting v3 images after v4 is published.
func (l *Library) Image(gameID string, version int, host string) (string, error) {
	if _, ok := l.games[gameID]; !ok {
		return "", fmt.Errorf("unknown game %q", gameID)
	}
	return ImageName(gameID, version, host), nil
}

// ImageName is the naming convention for built game images.
func ImageName(gameID string, version int, host string) string {
	return fmt.Sprintf("wge/%s:%d-%s", gameID, version, host)
}
