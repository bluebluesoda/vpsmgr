# Container Transparent Proxy

This add-on routes the vpsmgr container subnet through sing-box without
changing vpsmgr's own nftables table or the host's Tailscale rules.

The design is deliberately DNS-first:

1. Container TCP/UDP port 53 is redirected to the local sing-box DNS listener.
2. sing-box queries the configured mainland DoH resolver.
3. A response matching `geoip-cn` is returned as real addresses.
4. Any other A response is returned as fakeip; the real answer is never sent
   to the container.
5. Container traffic is source-routed into a dedicated sing-box TUN device.
6. CN addresses are direct, fakeip destinations are restored to their domain
   and sent through the Hysteria2 outbound, and other raw IP destinations use
   the proxy fallback.

No sniffing is required. IPv6/AAAA is intentionally disabled for the IPv4
container network.

## Files

- `config.json.template` — sing-box 1.14 configuration template.
- `vpsproxy.nft` — DNS-only nftables rules; the TUN handles other traffic.
- `vps-sing-box.service.template` — systemd unit and source policy routing.
The `geoip-cn.srs` rule-set is downloaded separately and is not committed.

The repository does not contain proxy credentials, proxy server addresses,
host addresses, or a generated production configuration.

## Prerequisites

- Incus bridge with an IPv4 subnet, normally `10.115.0.0/24`.
- Linux with nftables, policy routing, and `/dev/net/tun`.
- sing-box 1.14.x Linux amd64 binary, downloaded on a machine with access to
  the release site and copied to the host.
- A matching `geoip-cn.srs` rule-set copied to the host.
- A Hysteria2 server credential supplied through a private deployment method.

The host does not need to access GitHub. Download the binary and rule-set on a
workstation, verify their checksums, then use `scp`.

## Deployment

Copy the binary and rule-set to temporary paths on the host, then render all
`__PLACEHOLDER__` values in the three templates before installing them:

- `__DOH_IP__` — an IP address for the mainland DoH endpoint.
- `__DOH_SERVER_NAME__` — its TLS server name.
- `__PROXY_SERVER_IP__` — the Hysteria2 server IP.
- `__PROXY_SERVER_NAME__` — the Hysteria2 TLS server name.
- `__PROXY_PASSWORD__` — the Hysteria2 password.
- `__SUBNET__` — the vpsmgr container subnet.
- `__GATEWAY__` — the Incus bridge address.
- `__BRIDGE__` — normally `incusbr0`.
- `__EXT_IF__` — the host's default-route interface.
- `__EXT_GW__` — the host's default IPv4 gateway.

Install the rendered files as follows. The production config must remain
root-readable only by the sing-box group:

```sh
install -d -m 0755 /etc/sing-box /var/lib/sing-box
useradd --system --home-dir /var/lib/sing-box --shell /usr/sbin/nologin sing-box
install -o root -g root -m 0755 sing-box /usr/local/bin/sing-box
install -o root -g sing-box -m 0600 config.json /etc/sing-box/config.json
install -o root -g root -m 0644 geoip-cn.srs /etc/sing-box/geoip-cn.srs
install -o root -g root -m 0644 vpsproxy.nft /etc/sing-box/vpsproxy.nft
install -o root -g root -m 0644 vps-sing-box.service \
  /etc/systemd/system/vps-sing-box.service
chown -R sing-box:sing-box /var/lib/sing-box

/usr/local/bin/sing-box check -c /etc/sing-box/config.json
nft add table ip vpsproxy 2>/dev/null || true
nft -c -f /etc/sing-box/vpsproxy.nft
systemctl daemon-reload
systemctl enable --now vps-sing-box.service
```

Do not run `nft flush ruleset`. vpsmgr and Incus own other tables.

## Validation

From a running test container:

```sh
resolvectl query a-known-cn-domain.example
resolvectl query a-known-overseas-domain.example
getent ahostsv4 a-known-overseas-domain.example
curl -4 -I https://a-known-cn-domain.example
curl -4 -I https://a-known-overseas-domain.example
```

The overseas lookup must return only `198.18.0.0/15` fakeip addresses. Check
the route and service state on the host:

```sh
systemctl is-active vps-sing-box.service vps.service vps-nft.service incus.service
nft list table ip vpsproxy
nft list table inet vpsmgr
ip rule show
ip route show table 51821
journalctl -u vps-sing-box.service -e --no-pager
```

The TUN path is used instead of UDP TPROXY because some Linux/OpenWrt
combinations can mark and locally route a UDP TPROXY packet but fail to deliver
it to the transparent UDP socket. The TUN path avoids that kernel socket lookup
path while preserving the same routing policy.

## Rollback

Keep a pre-deployment backup of nftables, routes, systemd units, and the
vpsmgr configuration. To remove only this add-on:

```sh
systemctl disable --now vps-sing-box.service
rm -f /etc/systemd/system/vps-sing-box.service
systemctl daemon-reload
```

The service stop hook removes only `ip vpsproxy` and source policy table
`51821`. It does not delete or reload vpsmgr, Incus, or Tailscale rules.
