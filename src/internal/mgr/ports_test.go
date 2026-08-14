package mgr

import "testing"

func TestUserPorts(t *testing.T) {
	cases := []struct {
		start, perUser int
		want           string
	}{
		{10000, 100, "10000-10099"},
		{10700, 100, "10700-10799"},
		{29900, 100, "29900-29999"},
		{10000, 1, "10000"},
		{10000, 0, ""},
	}
	for _, c := range cases {
		if got := UserPorts(c.start, c.perUser); got != c.want {
			t.Errorf("UserPorts(%d, %d) = %q, want %q", c.start, c.perUser, got, c.want)
		}
	}
}

func TestUserPortsShort(t *testing.T) {
	cases := []struct {
		start int
		want  string
	}{
		{10000, "100xx"},
		{10700, "107xx"},
		{29900, "299xx"},
	}
	for _, c := range cases {
		if got := UserPortsShort(c.start); got != c.want {
			t.Errorf("UserPortsShort(%d) = %q, want %q", c.start, got, c.want)
		}
	}
}

func TestContainerIP(t *testing.T) {
	cases := []struct {
		subnet string
		idx    int
		want   string
	}{
		{"10.115.0.0/24", 1, "10.115.0.2"},
		{"10.115.0.0/24", 200, "10.115.0.201"},
		{"10.42.0.0/24", 5, "10.42.0.6"},
	}
	for _, c := range cases {
		got, err := ContainerIP(c.subnet, c.idx)
		if err != nil {
			t.Errorf("ContainerIP(%s, %d) error: %v", c.subnet, c.idx, err)
			continue
		}
		if got != c.want {
			t.Errorf("ContainerIP(%s, %d) = %q, want %q", c.subnet, c.idx, got, c.want)
		}
	}
	for _, bad := range []struct {
		subnet string
		idx    int
	}{
		{"10.115.0.0/16", 1}, // not a /24
		{"2001:db8::/64", 1}, // not IPv4
		{"10.115.0.0/24", 0}, // idx out of range
		{"10.115.0.0/24", 201},
	} {
		if _, err := ContainerIP(bad.subnet, bad.idx); err == nil {
			t.Errorf("ContainerIP(%s, %d): expected error", bad.subnet, bad.idx)
		}
	}
}
