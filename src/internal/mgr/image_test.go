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
			want: []ManagedImage{
				{Alias: "vpsmgr/debian-sshd", Label: "Debian 13",
					DescZh: "轻巧的原味 Debian 13 系统", DescEn: "Lightweight stock Debian 13"},
			},
		},
		{
			name:         "default plus alma",
			defaultAlias: "vpsmgr/debian-sshd",
			aliases:      []string{"vpsmgr/alma-sshd", "vpsmgr/debian-sshd"},
			want: []ManagedImage{
				{Alias: "vpsmgr/debian-sshd", Label: "Debian 13",
					DescZh: "轻巧的原味 Debian 13 系统", DescEn: "Lightweight stock Debian 13"},
				{Alias: "vpsmgr/alma-sshd", Label: "Alma 9",
					DescZh: "RHEL 复刻版镜像", DescEn: "RHEL-compatible rebuild"},
			},
		},
		{
			name:         "default first, rocky sorted after alma, dedupe",
			defaultAlias: "vpsmgr/debian-sshd",
			aliases:      []string{"vpsmgr/rocky-sshd", "vpsmgr/alma-sshd", "vpsmgr/alma-sshd"},
			want: []ManagedImage{
				{Alias: "vpsmgr/debian-sshd", Label: "Debian 13",
					DescZh: "轻巧的原味 Debian 13 系统", DescEn: "Lightweight stock Debian 13"},
				{Alias: "vpsmgr/alma-sshd", Label: "Alma 9",
					DescZh: "RHEL 复刻版镜像", DescEn: "RHEL-compatible rebuild"},
				{Alias: "vpsmgr/rocky-sshd", Label: "Rocky 9",
					DescZh: "RHEL 复刻版镜像", DescEn: "RHEL-compatible rebuild"},
			},
		},
		{
			name:         "default plus opensuse, sorted after alma",
			defaultAlias: "vpsmgr/debian-sshd",
			aliases:      []string{"vpsmgr/opensuse-sshd", "vpsmgr/debian-sshd"},
			want: []ManagedImage{
				{Alias: "vpsmgr/debian-sshd", Label: "Debian 13",
					DescZh: "轻巧的原味 Debian 13 系统", DescEn: "Lightweight stock Debian 13"},
				{Alias: "vpsmgr/opensuse-sshd", Label: "openSUSE Leap 16",
					DescZh: "SUSE 系发行版", DescEn: "SUSE-family distro"},
			},
		},
		{
			name:         "default plus debian dev, sorted after debian",
			defaultAlias: "vpsmgr/debian-sshd",
			aliases:      []string{"vpsmgr/debian-dev-sshd", "vpsmgr/debian-sshd"},
			want: []ManagedImage{
				{Alias: "vpsmgr/debian-sshd", Label: "Debian 13",
					DescZh: "轻巧的原味 Debian 13 系统", DescEn: "Lightweight stock Debian 13"},
				{Alias: "vpsmgr/debian-dev-sshd", Label: "Debian 13 dev",
					DescZh: "预装 Git、nvm、Node.js 24、Python 3、Go 等开发工具", DescEn: "Preinstalled dev tools: Git, nvm, Node.js 24, Python 3, Go"},
			},
		},
		{
			name:         "default missing locally still offered, unknown alias humanized, no desc",
			defaultAlias: "vpsmgr/debian-sshd",
			aliases:      []string{"vpsmgr/fedora-sshd"},
			want: []ManagedImage{
				{Alias: "vpsmgr/debian-sshd", Label: "Debian 13",
					DescZh: "轻巧的原味 Debian 13 系统", DescEn: "Lightweight stock Debian 13"},
				{Alias: "vpsmgr/fedora-sshd", Label: "fedora"},
			},
		},
		{
			name:         "arch with label and rolling-release desc",
			defaultAlias: "vpsmgr/debian-sshd",
			aliases:      []string{"vpsmgr/arch-sshd", "vpsmgr/debian-sshd"},
			want: []ManagedImage{
				{Alias: "vpsmgr/debian-sshd", Label: "Debian 13",
					DescZh: "轻巧的原味 Debian 13 系统", DescEn: "Lightweight stock Debian 13"},
				{Alias: "vpsmgr/arch-sshd", Label: "Arch Linux",
					DescZh: "滚动发行版，持续获取最新软件", DescEn: "Rolling release — always the latest packages"},
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
