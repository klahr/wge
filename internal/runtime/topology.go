package runtime

import (
	"fmt"
	"sort"

	"github.com/klahr/wge/internal/manifest"
)

// RunNetwork names the network a fully connected run shares.
func RunNetwork(runID int64) string {
	return fmt.Sprintf("wge-run-%d-net", runID)
}

// PairNetwork names the network joining exactly two hosts. The pair is sorted
// so both endpoints derive the same name.
func PairNetwork(runID int64, a, b string) string {
	if b < a {
		a, b = b, a
	}
	return fmt.Sprintf("wge-run-%d-%s-%s", runID, a, b)
}

// topology is the set of containers and networks one run needs.
//
// Every host is built when a player connects, not lazily when they reach a
// level on it. Pivoting is the point of a multi-host game, and `ssh vault` has
// to answer the first time it is typed -- a machine that materialises only
// after the player has already earned their way onto it is no use to them.
type topology struct {
	hosts    []string
	networks []string
	byHost   map[string][]string
}

// planTopology works out which hosts share which networks.
//
// A run whose hosts can all reach each other needs one network. One with
// declared peers gets a network per adjacent pair, because a Docker network is
// all-to-all and segmentation can only be expressed by which networks a
// container is on.
func planTopology(g *manifest.Game, runID int64) topology {
	hosts := g.HostIDs()
	sort.Strings(hosts)

	t := topology{hosts: hosts, byHost: map[string][]string{}}
	if len(hosts) < 2 {
		// A single machine talks to nobody, so it gets no network at all.
		return t
	}

	adjacent := g.Adjacency()
	if complete(hosts, adjacent) {
		name := RunNetwork(runID)
		t.networks = []string{name}
		for _, h := range hosts {
			t.byHost[h] = []string{name}
		}
		return t
	}

	seen := map[string]bool{}
	for i, a := range hosts {
		for _, b := range hosts[i+1:] {
			if !adjacent[a][b] {
				continue
			}
			name := PairNetwork(runID, a, b)
			if !seen[name] {
				seen[name] = true
				t.networks = append(t.networks, name)
			}
			t.byHost[a] = append(t.byHost[a], name)
			t.byHost[b] = append(t.byHost[b], name)
		}
	}
	sort.Strings(t.networks)
	return t
}

// complete reports whether every host can reach every other.
func complete(hosts []string, adjacent map[string]map[string]bool) bool {
	for i, a := range hosts {
		for _, b := range hosts[i+1:] {
			if !adjacent[a][b] {
				return false
			}
		}
	}
	return true
}

// isolated reports whether a host shares no network with anything.
func (t topology) isolated(host string) bool { return len(t.byHost[host]) == 0 }
