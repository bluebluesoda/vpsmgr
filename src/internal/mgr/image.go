package mgr

import (
	"sort"
	"strings"
)

// ManagedImage is one OS image offered for reinstall.
type ManagedImage struct {
	Alias string `json:"alias"` // Incus alias, e.g. "vpsmgr/debian-sshd"
	Label string `json:"label"` // display name, e.g. "Debian 13"
}

// imageLabels maps known managed image aliases to friendly display names.
var imageLabels = map[string]string{
	"vpsmgr/debian-sshd": "Debian 13",
	"vpsmgr/alma-sshd":   "Alma 9",
	"vpsmgr/rocky-sshd":  "Rocky 9",
}

// collectManagedImages picks the reinstallable images out of a raw alias list:
// the default image always comes first (it is available even as a remote
// fallback), then every other existing `vpsmgr/*-sshd` alias, deduplicated and
// alphabetical. Unknown managed aliases get a humanized label
// ("vpsmgr/fedora-sshd" -> "fedora").
func collectManagedImages(defaultAlias string, aliases []string) []ManagedImage {
	seen := map[string]bool{}
	var out []ManagedImage
	add := func(a string) {
		if seen[a] {
			return
		}
		seen[a] = true
		label, ok := imageLabels[a]
		if !ok {
			label = strings.TrimSuffix(strings.TrimPrefix(a, "vpsmgr/"), "-sshd")
		}
		out = append(out, ManagedImage{Alias: a, Label: label})
	}
	add(defaultAlias)
	sort.Strings(aliases)
	for _, a := range aliases {
		if strings.HasPrefix(a, "vpsmgr/") {
			add(a)
		}
	}
	return out
}

// Images returns the OS images offered for reinstall: the default Debian image
// first, then every other managed prebuilt image present in Incus (e.g. Alma 9
// built by scripts/60-rhel-image.sh). If Incus cannot be queried, only the
// default is offered so reinstall still works.
func (m *Manager) Images() []ManagedImage {
	aliases, err := m.lx.ImageAliases()
	if err != nil {
		aliases = nil
	}
	return collectManagedImages(m.cfg.Incus.Image, aliases)
}
