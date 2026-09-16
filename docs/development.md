# Development

## Building

`./build.sh [VERSION]` compiles the Go binary into `bin/vps`. It requires
any Go that supports toolchain auto-switch (≥ 1.21); `src/go.mod` pins the
exact toolchain (go1.26.5).

```sh
./build.sh                 # current OS/arch
GOOS=linux GOARCH=arm64 ./build.sh   # cross-compile
```

**Always `CGO_ENABLED=0`** (build.sh and the release workflow do this). The
project is pure Go with no cgo; leaving cgo on pulls in Go's `runtime/cgo`
bootstrap and a C toolchain whenever the standard `net` package is built,
which fails on machines with a broken/absent gcc. Release binaries are
therefore static and toolchain-free.

## Testing

```sh
cd src
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test ./...
```

The CI release workflow also runs vet+test with `CGO_ENABLED=0` on amd64.

### Developing against Incus

The panel talks to Incus over its Unix socket (`/var/lib/incus/unix.socket`,
group `incus-admin`). For a local dev loop:

```sh
# add the Zabbly LTS repo and install Incus (Debian 12/13, Ubuntu 22.04+)
curl -fsSL https://pkgs.zabbly.com/key.asc -o /etc/apt/keyrings/zabbly.asc
echo 'deb [signed-by=/etc/apt/keyrings/zabbly.asc] https://pkgs.zabbly.com/incus/lts-7.0/ trixie main' > /etc/apt/sources.list.d/zabbly-incus.list
apt-get update && apt-get install -y incus-base   # container-only build (no VM stack);
# install the full `incus` package only if you also develop against VMs
incus admin init --preseed < ...   # or use scripts/10-incus.sh
usermod -aG incus-admin "$USER"    # socket access without root
```

`vps install` does all of this on a real host; the exec API is exercised by
`internal/lx` over websockets (no `incus` CLI needed at runtime).

## Releasing

Tag a version (`v*`); the `.github/workflows/release.yml` release action builds
amd64/arm64 with `CGO_ENABLED=0`, runs vet+test, attests build provenance (SLSA)
and uploads a release with checksums. `install.sh` downloads the prebuilt binary
from the latest release, falling back to a local build.

The release page's notes are the **tag's own annotation** — write them at tag
time with an annotated tag:

```sh
git tag -a v1.8.0 -m "知识库 + 快照分享

- 管理员可编写 Markdown 知识库文章，用户端可阅读
- 用户可分享检查点，他人凭分享码重装"
git push origin v1.8.0
```

A **lightweight** tag (`git tag v1.8.0`, no `-a`) carries no message, so the
release body falls back to `fix some bugs`. Either way GitHub appends its
auto-generated "Full changelog" compare link below the notes.

## Conventions

- Small, single-purpose commits — one commit per bug/feature.
- Commit messages follow the existing style (short subject, then a bullet
  list of what and why).
- Shell scripts: `set -uo pipefail`, helpers `log`/`die`/`warn`, bash `-n`
  checked, Python one-liners used only where the stdlib is enough.
- The panel templates embed two languages (`zh` / `en`) via `{{if eq .Lang
  "zh"}}...{{else}}...{{end}}`.

## Repository layout

```
install.sh / uninstall.sh / build.sh   # lifecycle + local build
upgrade-haproxy.sh                    # follow the pinned HAProxy branch (patches)
scripts/   00-check 10-incus 20-network 30-haproxy 40-panel 50-image 60-rhel 70-opensuse 80-debian-dev 90-arch
configs/   reference configs (haproxy / systemd / sudoers)
docs/      this documentation
src/       Go source (single binary: CLI + panel)
```

## Domain proxy (HAProxy) — pinned branch

The proxy is HAProxy **3.4 LTS** from the vendors' performance packages
(`haproxy-awslc`). The project pins the BRANCH and follows its newest patch.

The branch appears in exactly three places — change all of them together:

| Where | Constant |
|---|---|
| `scripts/30-haproxy.sh` | `HAPROXY_BRANCH` / `HAPROXY_BRANCH_RE` / `HAPROXY_REPO_SLUG` |
| `upgrade-haproxy.sh` | the same three |
| `src/internal/cfg/cfg.go` | `HaproxyBranch` |

`install.sh --update` upgrades the panel binary and then runs
`upgrade-haproxy.sh`, which validates the candidate configuration with
`haproxy -c` before reloading (`systemctl reload`, never `restart`, so
established connections drain instead of dropping).

Runtime configuration is published as immutable **generations**
(`internal/hpx`): render the whole set from the DB, validate, then atomically
repoint `/etc/haproxy/current`. Never edit a generation in place, and never
write domain-specific settings into `/etc/haproxy/haproxy.cfg` (the static
entry config a reload cannot change).

The panel's `internal/hpx` renderer and `configs/haproxy/proxy.cfg` (the
bootstrap generation used before the first sync) are two copies of the same
template — keep them in step.
