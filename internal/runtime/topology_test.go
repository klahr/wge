package runtime

import (
	"testing"

	"github.com/klahr/wge/internal/manifest"
)

func gameWithHosts(hosts ...*manifest.Host) *manifest.Game {
	return &manifest.Game{ID: "heist", Version: 1, Hosts: hosts}
}

// A single machine talks to nobody and needs no network.
func TestSingleHostRunHasNoNetwork(t *testing.T) {
	plan := planTopology(gameWithHosts(), 1)

	if len(plan.hosts) != 1 || plan.hosts[0] != manifest.DefaultHostID {
		t.Fatalf("hosts = %v", plan.hosts)
	}
	if len(plan.networks) != 0 {
		t.Fatalf("networks = %v, want none", plan.networks)
	}
	if !plan.isolated(manifest.DefaultHostID) {
		t.Error("a lone host should be isolated")
	}
}

// A run whose hosts can all reach each other needs exactly one network.
func TestFullyConnectedRunSharesOneNetwork(t *testing.T) {
	g := gameWithHosts(
		&manifest.Host{ID: "relay"},
		&manifest.Host{ID: "office"},
		&manifest.Host{ID: "vault"},
	)
	plan := planTopology(g, 7)

	if len(plan.networks) != 1 || plan.networks[0] != RunNetwork(7) {
		t.Fatalf("networks = %v, want one run network", plan.networks)
	}
	for _, h := range plan.hosts {
		if got := plan.byHost[h]; len(got) != 1 || got[0] != RunNetwork(7) {
			t.Errorf("%s joins %v", h, got)
		}
	}
}

// Declared peers mean segmentation, which a Docker network can only express by
// membership: one network per adjacent pair.
func TestDeclaredPeersProduceOneNetworkPerEdge(t *testing.T) {
	g := gameWithHosts(
		&manifest.Host{ID: "relay", Peers: []string{"office"}},
		&manifest.Host{ID: "office", Peers: []string{"vault"}},
		&manifest.Host{ID: "vault"},
	)
	plan := planTopology(g, 3)

	if len(plan.networks) != 2 {
		t.Fatalf("networks = %v, want two edges", plan.networks)
	}
	// The relay reaches the office and nothing else; the office is the bridge.
	if got := len(plan.byHost["relay"]); got != 1 {
		t.Errorf("relay joins %d networks, want 1", got)
	}
	if got := len(plan.byHost["office"]); got != 2 {
		t.Errorf("office joins %d networks, want 2", got)
	}
	if got := len(plan.byHost["vault"]); got != 1 {
		t.Errorf("vault joins %d networks, want 1", got)
	}

	// And the two ends never share one, so the relay cannot reach the vault.
	for _, a := range plan.byHost["relay"] {
		for _, b := range plan.byHost["vault"] {
			if a == b {
				t.Fatalf("relay and vault share network %s; segmentation is not happening", a)
			}
		}
	}
}

// Both endpoints have to derive the same name or they land on two networks.
func TestPairNetworkNameIsOrderIndependent(t *testing.T) {
	if PairNetwork(1, "vault", "relay") != PairNetwork(1, "relay", "vault") {
		t.Fatal("pair network name depends on argument order")
	}
	if PairNetwork(1, "a", "b") == PairNetwork(2, "a", "b") {
		t.Fatal("two runs share a network name")
	}
}

// A host nobody named and which named nobody is still reachable, because a
// game that says nothing about its network means an open one.
func TestHostWithoutPeersJoinsTheOpenNetwork(t *testing.T) {
	g := gameWithHosts(
		&manifest.Host{ID: "relay"},
		&manifest.Host{ID: "vault"},
	)
	plan := planTopology(g, 1)

	if len(plan.networks) != 1 {
		t.Fatalf("networks = %v", plan.networks)
	}
	if plan.isolated("relay") || plan.isolated("vault") {
		t.Error("hosts that declared nothing should share a network")
	}
}
