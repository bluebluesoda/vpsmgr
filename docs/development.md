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

## Releasing

Tag a version (`v*`); the `.github/workflows/release.yml` builds amd64/arm64
with `CGO_ENABLED=0`, runs vet+test, attests build provenance (SLSA) and
uploads a release with checksums. `install.sh` downloads the prebuilt binary
from the latest release, falling back to a local build.

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
scripts/   00-check 10-lxd 20-network 30-traefik 40-panel 50-image
configs/   reference configs (traefik / systemd)
docs/      this documentation
src/       Go source (single binary: CLI + panel)
```
