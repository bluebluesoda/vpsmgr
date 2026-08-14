package mgr

import "testing"

func TestCollectManagedImages(t *testing.T) {
	cases := []struct {
		name         string
		defaultAlias string
		aliases      []string
		want         []ManagedImage
	}{
		{
			name:         "default only",
			defaultAlias: "vpsmgr/debian-sshd",
			aliases:      []string{"vpsmgr/debian-sshd"},
			want:         []ManagedImage{{"vpsmgr/debian-sshd", "Debian 13"}},
		},
		{
			name:         "default plus alma",
			defaultAlias: "vpsmgr/debian-sshd",
			aliases:      []string{"vpsmgr/alma-sshd", "vpsmgr/debian-sshd"},
			want: []ManagedImage{
				{"vpsmgr/debian-sshd", "Debian 13"},
				{"vpsmgr/alma-sshd", "Alma 9"},
			},
		},
		{
			name:         "default first, rocky sorted after alma, dedupe",
			defaultAlias: "vpsmgr/debian-sshd",
			aliases:      []string{"vpsmgr/rocky-sshd", "vpsmgr/alma-sshd", "vpsmgr/alma-sshd"},
			want: []ManagedImage{
				{"vpsmgr/debian-sshd", "Debian 13"},
				{"vpsmgr/alma-sshd", "Alma 9"},
				{"vpsmgr/rocky-sshd", "Rocky 9"},
			},
		},
		{
			name:         "default missing locally still offered, unknown alias humanized",
			defaultAlias: "vpsmgr/debian-sshd",
			aliases:      []string{"vpsmgr/fedora-sshd"},
			want: []ManagedImage{
				{"vpsmgr/debian-sshd", "Debian 13"},
				{"vpsmgr/fedora-sshd", "fedora"},
			},
		},
	}
	for _, c := range cases {
		got := collectManagedImages(c.defaultAlias, c.aliases)
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %d images %+v, want %d %+v", c.name, len(got), got, len(c.want), c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: image[%d] = %+v, want %+v", c.name, i, got[i], c.want[i])
			}
		}
	}
}
