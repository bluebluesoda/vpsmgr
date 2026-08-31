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
					continue
				}
				// A config write is atomic from the daemon's point of view in
				// normal operation, but tolerate the brief empty/truncated
				// window and keep the last known-good rules.
				if time.Since(lastRuleError) >= 10*time.Second {
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
	if srcIP.Equal(net.IPv6zero) {
		// DAD solicitations have no source address and must be multicast.
		dstIP = net.ParseIP("ff02::1")
		copy(dstMAC[:], []byte{0x33, 0x33, 0x00, 0x00, 0x00, 0x01})
	}
	target16 := target.To16()
	if target16 == nil {
		return nil, emptyMAC, fmt.Errorf("invalid IPv6 neighbor target %q", target)
	}

	icmp := make([]byte, 32)
	icmp[0] = icmpv6NA
	// icmp[1] is the code, which is zero.
	flags := naFlags
	if srcIP.Equal(net.IPv6zero) {
		// The solicited flag must be clear for a DAD response.
		flags = 0xc0000000
	}
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
	copy(ipv6[24:40], dstIP.To16())

	pseudo := make([]byte, 40)
	copy(pseudo[0:16], target16)
	copy(pseudo[16:32], dstIP.To16())
	binary.BigEndian.PutUint32(pseudo[32:36], uint32(len(icmp)))
	pseudo[39] = 58
	binary.BigEndian.PutUint16(icmp[2:4], checksum(append(pseudo, icmp...)))

	frame := make([]byte, 0, ethernetHeaderLen+len(ipv6)+len(icmp))
	frame = append(frame, dstMAC[:]...)
	frame = append(frame, sourceMAC[:]...)
	frame = append(frame, 0x86, 0xdd)
	frame = append(frame, ipv6...)
	frame = append(frame, icmp...)

	return frame, dstMAC, nil
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
