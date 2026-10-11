// Package ndp implements the small IPv6 neighbour responder vpsmgr needs for
// routed prefixes.  Linux's proxy_ndp and ndppd answer with a link-local
// source address.  Some providers reject that response and only accept a
// neighbour advertisement whose source is the advertised global address, so
// vpsmgr emits the advertisement directly on the external Ethernet link.
package ndp

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	ethernetHeaderLen = 14
	ipv6HeaderLen     = 40
	icmpv6NS          = 135
	icmpv6NA          = 136
	etherTypeIPv6     = 0x86dd

	// Router, solicited and override. This matches the advertisement emitted
	// by a directly-configured IPv6 address on Linux.
	naFlags = uint32(0xe0000000)

	// naFlagsUnsolicited is used for a gratuitous (unsolicited) advertisement:
	// the Solicited flag is clear because nothing solicited it (RFC 4861
	// §4.4), and Override is set so a router replaces even a *REACHABLE* entry
	// it already holds for the address. Override is what repairs an upstream
	// whose cache points at the wrong MAC — the case that otherwise black-holes
	// a container's off-link traffic until the router re-probes on its own.
	naFlagsUnsolicited = uint32(0xa0000000)

	// announceInterval is how often every known block is re-announced while the
	// responder is idle. A block that first appears is announced immediately,
	// regardless of this interval.
	announceInterval = 60 * time.Second
)

// Run listens for IPv6 Neighbor Solicitations on external and emits a
// Neighbor Advertisement for targets covered by the CIDR rules in configPath.
// allowed, when non-nil, is the operator's routed prefix: the responder only
// ever answers for a target inside it (and ignores any rule file entry outside
// it). This confines the root raw-socket listener to the operator's own address
// space, so a compromised writer of the rules file cannot turn the host into an
// NDP spoofer for arbitrary external addresses.
//
// The rules file is reread at most once per second while idle, and its mtime is
// checked when an NS arrives. This makes an add/del take effect for that very
// first solicitation without restarting this long-running process.
func Run(configPath, external string, allowed *net.IPNet) error {
	iface, err := net.InterfaceByName(external)
	if err != nil {
		return fmt.Errorf("find external interface %s: %w", external, err)
	}
	if len(iface.HardwareAddr) != 6 {
		return fmt.Errorf("external interface %s has no Ethernet MAC", external)
	}

	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(etherTypeIPv6)))
	if err != nil {
		return fmt.Errorf("open IPv6 packet socket: %w", err)
	}
	defer unix.Close(fd)
	bind := &unix.SockaddrLinklayer{Ifindex: iface.Index, Protocol: htons(etherTypeIPv6)}
	if err := unix.Bind(fd, bind); err != nil {
		return fmt.Errorf("bind IPv6 packet socket to %s: %w", external, err)
	}
	// A timeout lets us reload the rules and observe a clean shutdown signal
	// without needing a second control socket.
	tv := unix.NsecToTimeval(int64(time.Second))
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return fmt.Errorf("set packet socket timeout: %w", err)
	}

	mac := [6]byte{}
	copy(mac[:], iface.HardwareAddr)
	var rules []net.IPNet
	var rulesAt time.Time
	var rulesModTime time.Time
	var lastRuleError time.Time
	announced := &announceState{last: map[string]time.Time{}}
	packet := make([]byte, 4096)
	for {
		if time.Since(rulesAt) >= time.Second {
			next, loadErr := loadRules(configPath)
			if loadErr != nil {
				// Removing the file is the normal representation of an empty
				// container set. Clear the old rules immediately, otherwise a
				// deleted container would remain advertised until this process
				// restarted. A malformed/read-failed file is different: keep the
				// last known-good rules while the writer finishes an update.
				if errors.Is(loadErr, errNoRules) || errors.Is(loadErr, os.ErrNotExist) {
					rules = nil
					rulesAt = time.Now()
					rulesModTime = time.Time{}
				} else if time.Since(lastRuleError) >= 10*time.Second {
					// A config write is atomic from the daemon's point of view
					// in normal operation, but tolerate the brief empty or
					// truncated window and keep the last known-good rules.
					log.Printf("IPv6 NDP rule reload: %v", loadErr)
					lastRuleError = time.Now()
				}
			} else {
				rules = filterRules(next, allowed)
				rulesAt = time.Now()
				if info, statErr := os.Stat(configPath); statErr == nil {
					rulesModTime = info.ModTime()
				}
			}
			// Announcement pass, at most once a second: a block that just
			// appeared is announced at once, and every block is refreshed every
			// announceInterval. This is what lets an upstream router whose
			// neighbour cache for a container is stale or missing recover
			// immediately, instead of black-holing the container's off-link
			// traffic until its own slow neighbour probe happens to succeed.
			announced.sync(rules, func(target net.IP) error {
				return sendUnsolicitedAdvertisement(fd, iface.Index, mac, target)
			})
		}

		n, _, recvErr := unix.Recvfrom(fd, packet, 0)
		if recvErr != nil {
			if recvErr == unix.EAGAIN || recvErr == unix.EWOULDBLOCK || recvErr == unix.EINTR {
				continue
			}
			return fmt.Errorf("receive IPv6 packet: %w", recvErr)
		}
		if n < ethernetHeaderLen+ipv6HeaderLen+24 || len(rules) == 0 {
			continue
		}
		frame := packet[:n]
		if binary.BigEndian.Uint16(frame[12:14]) != etherTypeIPv6 || frame[20] != 58 {
			continue
		}
		// ICMPv6 starts immediately after the fixed IPv6 header. Extension
		// headers do not occur on Neighbor Solicitations.
		icmp := frame[ethernetHeaderLen+ipv6HeaderLen:]
		if len(icmp) < 24 || icmp[0] != icmpv6NS || icmp[1] != 0 {
			continue
		}
		// A new rule can be written immediately before the first external NS,
		// while the one-second periodic reload is still waiting. Notice an
		// atomic rename here so that first NS is answered instead of being
		// silently dropped and forcing the client to retry.
		if info, statErr := os.Stat(configPath); statErr == nil {
			if !info.ModTime().Equal(rulesModTime) {
				if next, loadErr := loadRules(configPath); loadErr == nil {
					rules = filterRules(next, allowed)
					rulesAt = time.Now()
					rulesModTime = info.ModTime()
				} else if errors.Is(loadErr, errNoRules) || errors.Is(loadErr, os.ErrNotExist) {
					rules = nil
					rulesAt = time.Now()
					rulesModTime = time.Time{}
				}
			}
		} else if errors.Is(statErr, os.ErrNotExist) && !rulesModTime.IsZero() {
			rules = nil
			rulesAt = time.Now()
			rulesModTime = time.Time{}
		}
		target := net.IP(append([]byte(nil), icmp[8:24]...))
		if !matches(target, rules) {
			continue
		}
		if err := sendAdvertisement(fd, iface.Index, mac, frame, target); err != nil {
			return fmt.Errorf("send NDP advertisement for %s: %w", target, err)
		}
	}
}

func loadRules(path string) ([]net.IPNet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var rules []net.IPNet
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 2 || fields[0] != "rule" {
			continue
		}
		_, network, err := net.ParseCIDR(fields[1])
		if err != nil || network.IP.To4() != nil {
			continue
		}
		rules = append(rules, *network)
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("%w: %s", errNoRules, path)
	}
	return rules, nil
}

var errNoRules = errors.New("no IPv6 rules")

// filterRules drops any rule not contained in allowed. When allowed is nil
// every rule is kept (tests / unconstrained mode).
func filterRules(rules []net.IPNet, allowed *net.IPNet) []net.IPNet {
	if allowed == nil {
		return rules
	}
	out := rules[:0]
	for _, r := range rules {
		if r.IP != nil && allowed.Contains(r.IP) {
			out = append(out, r)
		}
	}
	return out
}

func matches(target net.IP, rules []net.IPNet) bool {
	for i := range rules {
		if rules[i].Contains(target) {
			return true
		}
	}
	return false
}

func sendAdvertisement(fd, ifindex int, sourceMAC [6]byte, ns []byte, target net.IP) error {
	frame, dstMAC, err := buildAdvertisement(sourceMAC, ns, target)
	if err != nil {
		return err
	}
	return sendFrame(fd, ifindex, frame, dstMAC)
}

// sendUnsolicitedAdvertisement announces target as reachable at sourceMAC with
// a gratuitous advertisement sent to the all-nodes group, so every node on the
// link — the upstream router included — can (re)learn the mapping without
// having to solicit first.
func sendUnsolicitedAdvertisement(fd, ifindex int, sourceMAC [6]byte, target net.IP) error {
	frame, dstMAC, err := buildUnsolicitedAdvertisement(sourceMAC, target)
	if err != nil {
		return err
	}
	return sendFrame(fd, ifindex, frame, dstMAC)
}

func sendFrame(fd, ifindex int, frame []byte, dstMAC [6]byte) error {
	addr := &unix.SockaddrLinklayer{Ifindex: ifindex, Halen: 6, Protocol: htons(etherTypeIPv6)}
	copy(addr.Addr[:], dstMAC[:])
	return unix.Sendto(fd, frame, 0, addr)
}

func buildAdvertisement(sourceMAC [6]byte, ns []byte, target net.IP) ([]byte, [6]byte, error) {
	var emptyMAC [6]byte
	if len(ns) < ethernetHeaderLen+ipv6HeaderLen+24 {
		return nil, emptyMAC, fmt.Errorf("short neighbor solicitation")
	}
	var dstMAC [6]byte
	copy(dstMAC[:], ns[6:12])
	srcIP := net.IP(ns[22:38])
	dstIP := srcIP
	flags := naFlags
	if srcIP.Equal(net.IPv6zero) {
		// DAD solicitations have no source address and must be multicast.
		dstIP = net.ParseIP("ff02::1")
		dstMAC = [6]byte{0x33, 0x33, 0x00, 0x00, 0x00, 0x01}
		// The solicited flag must be clear for a DAD response.
		flags = 0xc0000000
	}
	target16 := target.To16()
	if target16 == nil {
		return nil, emptyMAC, fmt.Errorf("invalid IPv6 neighbor target %q", target)
	}
	frame, err := buildNA(sourceMAC, target16, dstIP.To16(), dstMAC, flags)
	if err != nil {
		return nil, emptyMAC, err
	}
	return frame, dstMAC, nil
}

// buildUnsolicitedAdvertisement builds a gratuitous advertisement for target,
// addressed to the all-nodes multicast group.
func buildUnsolicitedAdvertisement(sourceMAC [6]byte, target net.IP) ([]byte, [6]byte, error) {
	target16 := target.To16()
	if target16 == nil {
		return nil, [6]byte{}, fmt.Errorf("invalid IPv6 neighbor target %q", target)
	}
	dstMAC := [6]byte{0x33, 0x33, 0x00, 0x00, 0x00, 0x01} // ff02::1
	frame, err := buildNA(sourceMAC, target16, net.ParseIP("ff02::1").To16(), dstMAC, naFlagsUnsolicited)
	if err != nil {
		return nil, [6]byte{}, err
	}
	return frame, dstMAC, nil
}

// buildNA assembles a Neighbor Advertisement frame: the advertised target is
// the IPv6 source, dstIP/dstMAC is the destination, and flags carries the
// R/S/O bits.
func buildNA(sourceMAC [6]byte, target16, dstIP16 net.IP, dstMAC [6]byte, flags uint32) ([]byte, error) {
	icmp := make([]byte, 32)
	icmp[0] = icmpv6NA
	// icmp[1] is the code, which is zero.
	binary.BigEndian.PutUint32(icmp[4:8], flags)
	copy(icmp[8:24], target16)
	icmp[24] = 2 // Target Link-Layer Address option.
	icmp[25] = 1 // One 8-byte option unit.
	copy(icmp[26:32], sourceMAC[:])

	ipv6 := make([]byte, ipv6HeaderLen)
	ipv6[0] = 0x60
	binary.BigEndian.PutUint16(ipv6[4:6], uint16(len(icmp)))
	ipv6[6] = 58
	ipv6[7] = 255
	copy(ipv6[8:24], target16)
	copy(ipv6[24:40], dstIP16)

	pseudo := make([]byte, 40)
	copy(pseudo[0:16], target16)
	copy(pseudo[16:32], dstIP16)
	binary.BigEndian.PutUint32(pseudo[32:36], uint32(len(icmp)))
	pseudo[39] = 58
	binary.BigEndian.PutUint16(icmp[2:4], checksum(append(pseudo, icmp...)))

	frame := make([]byte, 0, ethernetHeaderLen+len(ipv6)+len(icmp))
	frame = append(frame, dstMAC[:]...)
	frame = append(frame, sourceMAC[:]...)
	frame = append(frame, 0x86, 0xdd)
	frame = append(frame, ipv6...)
	frame = append(frame, icmp...)
	return frame, nil
}

// announceState records when each block was last announced so that a block
// which first appears is announced once immediately and every block is
// refreshed periodically.
type announceState struct {
	last map[string]time.Time
	at   time.Time
}

// sync announces the primary address of every rule that is new since the last
// pass, and re-announces every rule once announceInterval has elapsed. send is
// injected so the decision logic is unit-testable without a raw socket; a rule
// whose send fails stays unannounced and is retried on the next pass.
func (a *announceState) sync(rules []net.IPNet, send func(net.IP) error) {
	now := time.Now()
	present := make(map[string]bool, len(rules))
	refresh := now.Sub(a.at) >= announceInterval
	for i := range rules {
		key := rules[i].String()
		present[key] = true
		if _, seen := a.last[key]; seen && !refresh {
			continue
		}
		target := announceTarget(rules[i])
		if target == nil {
			continue
		}
		if err := send(target); err != nil {
			log.Printf("IPv6 NDP announcement for %s: %v", target, err)
			continue
		}
		a.last[key] = now
	}
	if refresh {
		a.at = now
	}
	// Forget blocks that are gone, so removing and re-adding one announces it
	// again instead of assuming it is still current.
	for key := range a.last {
		if !present[key] {
			delete(a.last, key)
		}
	}
}

// announceTarget is the address a block's announcement advertises: the block's
// first host address (its primary), or the address itself for a /128 rule.
func announceTarget(block net.IPNet) net.IP {
	if target := hostOffset(block.IP, 1); target != nil && block.Contains(target) {
		return target
	}
	if block.IP.To16() != nil {
		return block.IP
	}
	return nil
}

// hostOffset returns netAddr + k, incrementing the low 64 bits with carry. k
// only ever touches host bits, so the result stays inside the block.
func hostOffset(netAddr net.IP, k uint64) net.IP {
	base := netAddr.To16()
	if base == nil {
		return nil
	}
	ip := make(net.IP, 16)
	copy(ip, base)
	for i := 15; i >= 8 && k > 0; i-- {
		v := uint64(ip[i]) + (k & 0xff)
		ip[i] = byte(v & 0xff)
		k >>= 8
		if v > 0xff {
			k++
		}
	}
	return ip
}

func checksum(data []byte) uint16 {
	var sum uint32
	for len(data) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(data))
		data = data[2:]
	}
	if len(data) == 1 {
		sum += uint32(data[0]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func htons(v uint16) uint16 { return (v<<8)&0xff00 | v>>8 }
