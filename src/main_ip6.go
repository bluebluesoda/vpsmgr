package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"vpsmgr/internal/cfg"
)

// cmdIP6 is the root helper behind the sudoers whitelist entry
// `vps ALL=(root) NOPASSWD: /usr/local/bin/vps ip6 *`. The panel daemon runs
// unprivileged; instead of granting it bare `ip -6 ... *` (whose wildcard lets
// it run ip with ANY arguments — equivalent to arbitrary network control), it
// is limited to this subcommand, which validates every argument before
// touching the network (review P0-1 remaining item).
//
// The binary is root-owned (0755, installed by the root installer), so the
// panel user cannot tamper with the validation code itself.
//
// Supported operations (args: <op> <addr-or-cidr> <dev>):
//
//	route-add      <ipv6-cidr> <iface>     ip -6 route add <cidr> dev <iface>
//	addr-add       <ipv6-addr/prefix> <iface>  ip -6 addr add <addr> dev <iface>
//	route-del      <ipv6-addr> <iface>     ip -6 route del <addr>/128 dev <iface>
//	neigh-del-proxy <ipv6-addr> <iface>    ip -6 neigh del proxy <addr> dev <iface>
//
// Every non-op argument must be a valid IPv6 address/CIDR and a valid Linux
// interface name — nothing else can reach `ip`.
func cmdIP6(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: vps ip6 <route-add|addr-add|route-del|neigh-del-proxy> <addr-or-cidr> <iface>")
	}
	op, val, dev := args[0], args[1], args[2]

	// Interface names are validated first (same rules as config).
	if !cfg.ValidIfaceName(dev) {
		return fmt.Errorf("invalid interface name %q", dev)
	}

	switch op {
	case "route-add":
		// val must be a global-scope IPv6 CIDR.
		ip, ipnet, err := net.ParseCIDR(val)
		if err != nil || ip.To4() != nil {
			return fmt.Errorf("route-add: %q is not an IPv6 CIDR", val)
		}
		if !ipnet.IP.Equal(ip.Mask(ipnet.Mask)) {
			return fmt.Errorf("route-add: %q is not a network address", val)
		}
		return runIP("-6", "route", "add", val, "dev", dev)
	case "addr-add":
		// val must be an IPv6 address with a prefix (or bare address).
		ip, _, err := net.ParseCIDR(val)
		if err != nil {
			// Accept a bare IPv6 address (implicit /128).
			ip = net.ParseIP(val)
			if ip == nil || ip.To4() != nil {
				return fmt.Errorf("addr-add: %q is not an IPv6 address", val)
			}
		} else if ip.To4() != nil {
			return fmt.Errorf("addr-add: %q is not an IPv6 address", val)
		}
		return runIP("-6", "addr", "add", val, "dev", dev)
	case "route-del":
		// val must be an IPv6 address (the caller appends /128).
		ip := net.ParseIP(val)
		if ip == nil || ip.To4() != nil {
			return fmt.Errorf("route-del: %q is not an IPv6 address", val)
		}
		return runIP("-6", "route", "del", val+"/128", "dev", dev)
	case "neigh-del-proxy":
		ip := net.ParseIP(val)
		if ip == nil || ip.To4() != nil {
			return fmt.Errorf("neigh-del-proxy: %q is not an IPv6 address", val)
		}
		return runIP("-6", "neigh", "del", "proxy", val, "dev", dev)
	default:
		return fmt.Errorf("unknown ip6 operation %q", op)
	}
}

// runIP executes the ip command (this process is already root via sudo).
func runIP(args ...string) error {
	out, err := exec.Command("/sbin/ip", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("ip %s: %s", strings.Join(args, " "), msg)
		}
		return fmt.Errorf("ip %s: %v", strings.Join(args, " "), err)
	}
	return nil
}

// ensureBinaryIsRootOwned makes sure /usr/local/bin/vps is root-owned and not
// writable by the panel user. Called during `vps install` after the binary is
// in place; without this, the `vps ip6` sudoers entry could be subverted by a
// panel-user-writable binary.
func ensureBinaryIsRootOwned() error {
	if err := os.Chown("/usr/local/bin/vps", 0, 0); err != nil {
		return fmt.Errorf("chown /usr/local/bin/vps: %w", err)
	}
	if err := os.Chmod("/usr/local/bin/vps", 0o755); err != nil {
		return fmt.Errorf("chmod /usr/local/bin/vps: %w", err)
	}
	return nil
}
