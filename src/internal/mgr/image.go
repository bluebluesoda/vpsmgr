package mgr

import (
	"sort"
	"strings"
)

// ManagedImage is one OS image offered for reinstall.
type ManagedImage struct {
	Alias  string `json:"alias"`             // Incus alias, e.g. "vpsmgr/debian-sshd"
	Label  string `json:"label"`             // display name, e.g. "Debian 13"
	DescZh string `json:"desc_zh,omitempty"` // short one-line description (Chinese)
	DescEn string `json:"desc_en,omitempty"` // short one-line description (English)
	// Version is the image's Incus description — for the rolling Arch build the
	// 8-digit build date YYYYMMDD (e.g. "20260824") — which the reinstall dialog
	// folds into the Arch intro so the snapshot's age is visible. Empty for
	// images without one.
	Version string `json:"version,omitempty"`
}

// imageLabels maps known managed image aliases to friendly display names.
var imageLabels = map[string]string{
	"vpsmgr/debian-sshd":     "Debian 13",
	"vpsmgr/debian-dev-sshd": "Debian 13 dev",
	"vpsmgr/alma-sshd":       "Alma 9",
	"vpsmgr/rocky-sshd":      "Rocky 9",
	"vpsmgr/opensuse-sshd":   "openSUSE Leap 16",
	"vpsmgr/arch-sshd":       "Arch Linux",
}

// imageDesc maps known managed image aliases to a short one-line blurb shown in
// the reinstall dialog when the image is selected. First entry is Chinese,
// second English. Unknown aliases get no description (the UI shows nothing).
var imageDesc = map[string][2]string{
	"vpsmgr/debian-sshd":     {"轻巧的原味 Debian 13 系统", "Lightweight stock Debian 13"},
	"vpsmgr/debian-dev-sshd": {"预装 Git、nvm、Node.js 24、Python 3、Go 等开发工具", "Preinstalled dev tools: Git, nvm, Node.js 24, Python 3, Go"},
	"vpsmgr/alma-sshd":       {"RHEL 复刻版镜像", "RHEL-compatible rebuild"},
	"vpsmgr/rocky-sshd":      {"RHEL 复刻版镜像", "RHEL-compatible rebuild"},
	"vpsmgr/opensuse-sshd":   {"SUSE 系发行版", "SUSE-family distro"},
	"vpsmgr/arch-sshd":       {"滚动发行版，持续获取最新软件", "Rolling release — always the latest packages"},
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
		desc := imageDesc[a]
		out = append(out, ManagedImage{Alias: a, Label: label, DescZh: desc[0], DescEn: desc[1]})
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
// default is offered so reinstall still works. Each image carries its Incus
// description as Version (the Arch rolling build records the snapshot's
// YYYYMMDD build date there), folded into the Arch intro by decorateImageDesc.
func (m *Manager) Images() []ManagedImage {
	infos, err := m.lx.ImageAliasesWithDesc()
	if err != nil {
		infos = nil
	}
	desc := map[string]string{}
	aliases := make([]string, 0, len(infos))
	for _, info := range infos {
		desc[info.Alias] = info.Description
		aliases = append(aliases, info.Alias)
	}
	out := collectManagedImages(m.cfg.Incus.Image, aliases)
	for i := range out {
		out[i].Version = desc[out[i].Alias]
		out[i] = decorateImageDesc(out[i])
	}
	return out
}

// decorateImageDesc surfaces a recorded build date in the image's intro line.
// The Arch image's Incus description holds the 8-digit build date, so its
// blurb becomes "…滚动发行版，我们提供镜像打包于 <YYYYMMDD>" — otherwise the
// static imageDesc blurb is used as-is.
func decorateImageDesc(m ManagedImage) ManagedImage {
	if m.Alias == "vpsmgr/arch-sshd" && m.Version != "" {
		m.DescZh = "Arch Linux 是一个滚动发行版，我们提供镜像打包于 " + m.Version
		m.DescEn = "Arch Linux is a rolling release; our image was built on " + m.Version
	}
	return m
}
