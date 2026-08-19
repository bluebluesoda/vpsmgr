package mgr

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

// HostMem describes the host's physical memory and swap usage.
type HostMem struct {
	MemTotal  uint64 // bytes
	MemUsed   uint64 // bytes
	SwapTotal uint64 // bytes
	SwapUsed  uint64 // bytes
}

// HostStats is the admin panel's host overview: memory, swap, pool space,
// system uptime and whether a reboot is pending (e.g. Ubuntu
// unattended-upgrades or livepatch staged a kernel update).
type HostStats struct {
	Mem          HostMem
	PoolTotal    int64         // bytes
	PoolUsed     int64         // bytes
	PoolAvail    int64         // bytes
	Uptime       time.Duration // host uptime since boot
	RebootNeeded bool
}

// PoolRemainingBytes returns the pool's total/used/available bytes via the Incus
// storage-pool resources API (one REST call, exact byte counts).
func (m *Manager) PoolRemainingBytes() (total, used, avail int64, err error) {
	total, used, err = m.lx.PoolResources(m.cfg.Incus.Pool)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("lxd storage resources %s: %w", m.cfg.Incus.Pool, err)
	}
	avail = total - used
	if avail < 0 {
		avail = 0
	}
	return total, used, avail, nil
}

// RefreshHostStats updates the latest in-memory host overview. Host history is
// intentionally not persisted; the cache only keeps page requests off Incus.
func (m *Manager) RefreshHostStats() {
	hs := HostStats{}
	hs.Mem = readMemInfo()
	total, used, avail, err := m.PoolRemainingBytes()
	if err == nil {
		hs.PoolTotal, hs.PoolUsed, hs.PoolAvail = total, used, avail
	}
	hs.Uptime = readUptime()
	hs.RebootNeeded = rebootRequired()
	m.hostMu.Lock()
	m.hostStats = hs
	m.hostReady = true
	m.hostMu.Unlock()
}

// HostStats returns the latest cached host overview. The cache is refreshed at
// startup and with each resource sample; no Incus request is made here.
func (m *Manager) HostStats() HostStats {
	m.hostMu.RLock()
	defer m.hostMu.RUnlock()
	if !m.hostReady {
		return HostStats{}
	}
	return m.hostStats
}

// readMemInfo parses /proc/meminfo into usable/available memory and swap.
func readMemInfo() HostMem {
	var m HostMem
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	kb := func(k string) uint64 {
		var v uint64
		fmt.Sscanf(k, "%d", &v)
		return v * 1024
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			m.MemTotal = kb(strings.TrimSpace(strings.TrimPrefix(line, "MemTotal:")))
		case strings.HasPrefix(line, "MemAvailable:"):
			m.MemUsed = m.MemTotal - kb(strings.TrimSpace(strings.TrimPrefix(line, "MemAvailable:")))
		case strings.HasPrefix(line, "SwapTotal:"):
			m.SwapTotal = kb(strings.TrimSpace(strings.TrimPrefix(line, "SwapTotal:")))
		case strings.HasPrefix(line, "SwapFree:"):
			m.SwapUsed = m.SwapTotal - kb(strings.TrimSpace(strings.TrimPrefix(line, "SwapFree:")))
		}
	}
	return m
}

// rebootRequired reports whether /var/run/reboot-required exists (set by
// Ubuntu unattended-upgrades / livepatch when a kernel update needs a reboot).
func rebootRequired() bool {
	_, err := os.Stat("/var/run/reboot-required")
	return err == nil
}

// readUptime parses /proc/uptime and returns the host uptime as a duration.
// A missing or malformed file is treated as 0 so the panel still renders.
func readUptime() time.Duration {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

// UserStatus is one row of the admin user table: the DB user record plus the
// latest persisted resource sample and five-minute CPU average.
type UserStatus struct {
	User     *db.User
	State    string
	CPUUse   string // e.g. "12%" or "-"
	MemUse   string // e.g. "345 MiB" or "-"
	DiskUsed string // actual disk usage, e.g. "184 MiB" or "-"
	UpGB     string
	DownGB   string
	IPv6     string
	Procs    int64 // latest sampled process count (0 when stopped / unavailable)
}

// BatchUsers reads the database-backed resource snapshot. It intentionally
// performs no Incus call: the background resource sampler owns collection.
func (m *Manager) BatchUsers() ([]*UserStatus, error) {
	users, err := m.db.ListUsers()
	if err != nil {
		return nil, err
	}
	latest, err := m.db.LatestResourceSamples()
	if err != nil {
		return nil, err
	}
	average, err := m.db.AverageCPU(time.Now().Add(-5 * time.Minute).Unix())
	if err != nil {
		return nil, err
	}
	bandwidth, err := m.db.AllBandwidth()
	if err != nil {
		return nil, err
	}

	out := make([]*UserStatus, 0, len(users))
	for _, u := range users {
		transfer := bandwidth[u.ID]
		up, down := transfer.Upload, transfer.Download
		rs := &UserStatus{User: u, State: "-", UpGB: FormatGB(up), DownGB: FormatGB(down)}
		if sample, ok := latest[u.ID]; ok {
			rs.State = resourceStateName(sample.State)
			if sample.State == resourceRunning {
				rs.Procs = sample.Processes
				rs.MemUse = humanBytes(sample.MemoryMiB * (1 << 20))
				rs.DiskUsed = humanBytes(sample.DiskUsedMiB * (1 << 20))
			} else {
				rs.MemUse = "-"
				rs.DiskUsed = "-"
			}
		} else {
			rs.MemUse = "-"
			rs.DiskUsed = "-"
		}
		if pct, ok := average[u.ID]; ok && latest[u.ID].State == resourceRunning {
			rs.CPUUse = fmt.Sprintf("%.0f%%", pct)
		} else {
			rs.CPUUse = "-"
		}
		if m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool {
			rs.IPv6 = u.IPv6Address
		} else {
			if ipv6, err := m.IPv6Addr(u.Name); err == nil {
				rs.IPv6 = ipv6
			}
		}
		out = append(out, rs)
	}
	return out, nil
}

func resourceStateName(state int) string {
	switch state {
	case resourceRunning:
		return "Running"
	case resourceStopped:
		return "Stopped"
	default:
		return "Unknown"
	}
}

// humanBytes renders a byte count as a short human string (e.g. "184 MiB").
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
