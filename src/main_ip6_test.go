package main

import "testing"

// parseNeighborEntry feeds the neigh-pin/neigh-unpin helpers: the MAC must be
// read correctly (a wrong one would pin the gateway to a black hole) and an
// unresolved entry must yield an empty lladdr so the helper leaves it alone.
func TestParseNeighborEntry(t *testing.T) {
	cases := []struct {
		name       string
		out        string
		wantState  string
		wantLladdr string
	}{
		{"reachable", "fe80::1 dev eth0 lladdr 00:00:5e:60:02:07 router REACHABLE \n", "REACHABLE", "00:00:5e:60:02:07"},
		{"permanent", "fe80::1 dev eth0 lladdr 00:00:5e:60:02:07 PERMANENT \n", "PERMANENT", "00:00:5e:60:02:07"},
		{"incomplete", "fe80::1 dev eth0 router INCOMPLETE \n", "INCOMPLETE", ""},
		{"absent", "", "", ""},
	}
	for _, tc := range cases {
		state, lladdr := parseNeighborEntry(tc.out)
		if state != tc.wantState || lladdr != tc.wantLladdr {
			t.Errorf("%s: got (%q,%q), want (%q,%q)", tc.name, state, lladdr, tc.wantState, tc.wantLladdr)
		}
	}
}
