package main

import (
	"testing"

	"github.com/klahr/wge/internal/runtime"
)

// A node's name is what a run's placement is recorded as, so a typo in the spec
// is a set of runs that go to the wrong machine or nowhere. It has to fail at
// startup, not at the first reconnection.
func TestParseNodes(t *testing.T) {
	cases := []struct {
		spec string
		want []runtime.Node
		bad  bool
	}{
		{spec: "", want: nil},
		{spec: "  ", want: nil},
		{
			spec: "relay=tcp://10.0.0.11:2375",
			want: []runtime.Node{{Name: "relay", Socket: "tcp://10.0.0.11:2375"}},
		},
		{
			spec: "a=/var/run/docker.sock, b=tcp://10.0.0.12:2375 ",
			want: []runtime.Node{
				{Name: "a", Socket: "/var/run/docker.sock"},
				{Name: "b", Socket: "tcp://10.0.0.12:2375"},
			},
		},
		// A trailing comma is a slip, not a nameless machine.
		{
			spec: "a=tcp://one:2375,",
			want: []runtime.Node{{Name: "a", Socket: "tcp://one:2375"}},
		},
		{spec: "tcp://10.0.0.11:2375", bad: true},
		{spec: "=tcp://10.0.0.11:2375", bad: true},
		{spec: "relay=", bad: true},
	}

	for _, c := range cases {
		got, err := parseNodes(c.spec)
		if c.bad {
			if err == nil {
				t.Errorf("%q was accepted as %v", c.spec, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.spec, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("%q gave %v, want %v", c.spec, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q node %d is %v, want %v", c.spec, i, got[i], c.want[i])
			}
		}
	}
}
