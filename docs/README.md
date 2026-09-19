# Documentation

Technical documentation for vpsmgr. All docs are in English.

| Doc | What it covers |
|---|---|
| [architecture.md](architecture.md) | System design: components, storage, network, security, traffic, users and user groups, image, install/uninstall lifecycle |
| [configuration.md](configuration.md) | Full reference for `/etc/vpsmgr/config.yaml` |
| [ipv6.md](ipv6.md) | IPv6 pass-through design: per-account blocks, prefix support, routing, installer flow |
| [ipv6-ndp-responder.md](ipv6-ndp-responder.md) | Why prefix mode replaced ndppd with an in-tree NDP responder, design hardening, and what to test at scale |
| [web-ssh.md](web-ssh.md) | The browser terminal: how it reaches a container, how a session survives a dropped connection, and how to disable it |
| [development.md](development.md) | Building, testing, releasing, conventions |

See also [`AGENTS.md`](../AGENTS.md) at the repo root — short version aimed at AI coding agents.
