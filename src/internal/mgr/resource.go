package mgr

import (
	"fmt"
	"math"
	"time"

	"vpsmgr/internal/db"
)

const (
	// ResourceSampleInterval is also the resolution of the persisted history.
	ResourceSampleInterval = time.Minute
	resourceRetention      = 7 * 24 * time.Hour

	resourceStopped = 0
	resourceRunning = 1
	resourceUnknown = 2
)

// SampleResources takes one combined Incus snapshot for all managed users.
// It deliberately does not use per-instance state or exec: one instances list
// and one metrics request replace the old per-container request fan-out.
func (m *Manager) SampleResources() error {
	m.sampleMu.Lock()
	defer m.sampleMu.Unlock()

	users, err := m.db.ListUsers()
	if err != nil {
		return err
	}
	statuses, err := m.lx.InstanceStatuses()
	if err != nil {
		return err
	}
	metrics, err := m.lx.Metrics()
	if err != nil {
		return err
	}
	previous, err := m.db.LatestResourceSamples()
	if err != nil {
		return err
	}

	sampleMinute := time.Now().Unix() / int64(ResourceSampleInterval/time.Second) * int64(ResourceSampleInterval/time.Second)
	samples := make([]db.ResourceSample, 0, len(users))
	observations := make([]db.BandwidthObservation, 0, len(users))
	for _, u := range users {
		s := db.ResourceSample{
			UserID:        u.ID,
			SampleMinute:  sampleMinute,
			CPUPercentX10: -1,
		}
		status, ok := statuses[u.Name]
		switch {
		case !ok:
			s.State = resourceUnknown
		case status == "Running":
			s.State = resourceRunning
		default:
			s.State = resourceStopped
		}

		metric, hasMetric := metrics[u.Name]
		if s.State == resourceRunning && hasMetric {
			s.BootTime = metric.BootTime
			s.CPUSecondsNS = secondsToNanos(metric.CPUSeconds)
			s.MemoryMiB = bytesToMiB(memoryUsed(metric.MemoryTotal, metric.MemoryAvailable))
			s.Processes = metric.Processes
			s.DiskUsedMiB = bytesToMiB(filesystemUsed(metric.FilesystemSize, metric.FilesystemAvail))
			s.RXBytesTotal = metric.RxBytes
			s.TXBytesTotal = metric.TxBytes
			s.DiskReadBytes = metric.DiskReadBytes
			s.DiskWrittenBytes = metric.DiskWriteBytes
			s.CPUPercentX10 = cpuPercentX10(u.CPU, s, previous[u.ID], sampleMinute)
			observations = append(observations, db.BandwidthObservation{
				UserID: u.ID, RX: metric.RxBytes, TX: metric.TxBytes, BootTime: metric.BootTime,
			})
		}
		samples = append(samples, s)
	}

	period := time.Now().UTC().Format("2006-01")
	cutoff := sampleMinute - int64(resourceRetention/time.Second)
	if err := m.db.RecordResourceSamples(samples, observations, period, cutoff); err != nil {
		return fmt.Errorf("record resource samples: %w", err)
	}
	return nil
}

func secondsToNanos(seconds float64) int64 {
	if seconds <= 0 {
		return 0
	}
	return int64(seconds * 1e9)
}

func memoryUsed(total, available int64) int64 {
	if total <= 0 || available < 0 || available > total {
		return 0
	}
	return total - available
}

func filesystemUsed(size, available int64) int64 {
	if size <= 0 || available < 0 || available > size {
		return 0
	}
	return size - available
}

func bytesToMiB(bytes int64) int64 {
	if bytes <= 0 {
		return 0
	}
	return (bytes + (1 << 20) - 1) / (1 << 20)
}

func cpuPercentX10(cpu int, current db.ResourceSample, previous db.ResourceSample, sampleMinute int64) int64 {
	if cpu <= 0 || previous.State != resourceRunning || current.BootTime == 0 ||
		previous.BootTime == 0 || current.BootTime != previous.BootTime ||
		previous.SampleMinute >= sampleMinute || current.CPUSecondsNS < previous.CPUSecondsNS {
		return -1
	}
	elapsed := sampleMinute - previous.SampleMinute
	if elapsed <= 0 {
		return -1
	}
	percent := float64(current.CPUSecondsNS-previous.CPUSecondsNS) / 1e9 /
		float64(elapsed) / (float64(cpu) / 10) * 100
	if percent < 0 || math.IsNaN(percent) || math.IsInf(percent, 0) {
		return -1
	}
	return int64(math.Round(percent * 10))
}

// ResourceHistory returns persisted samples for future resource charts.
func (m *Manager) ResourceHistory(userID int64, since time.Time) ([]db.ResourceSample, error) {
	return m.db.ResourceHistory(userID, since.Unix())
}

// ChartPoint is one minute of chart data for the user panel: CPU percent,
// memory in MiB, and the bytes transferred during that minute (download rx /
// upload tx), derived from the cumulative counter deltas between samples.
type ChartPoint struct {
	Minute int64   `json:"m"` // unix seconds, minute-floored
	CPU    float64 `json:"c"` // percent; -1 when unknown/not running
	Mem    int64   `json:"y"` // MiB; -1 when unknown/not running
	RX     int64   `json:"r"` // bytes downloaded during this minute
	TX     int64   `json:"t"` // bytes uploaded during this minute
}

// ChartHistory returns up to 24h of per-minute chart data for one user. It is
// a pure DB read (no Incus call): cpu_pct_x10 is stored per-minute, and the
// per-minute bandwidth is the delta of the cumulative rx/tx counters between
// consecutive samples. A counter reset or container restart (boot time
// change) yields a 0-delta for that minute rather than a bogus spike.
func (m *Manager) ChartHistory(userID int64, since time.Time) ([]ChartPoint, error) {
	samples, err := m.db.ResourceHistory(userID, since.Unix())
	if err != nil {
		return nil, err
	}
	out := make([]ChartPoint, 0, len(samples))
	var prev *db.ResourceSample
	for i := range samples {
		s := &samples[i]
		p := ChartPoint{Minute: s.SampleMinute, CPU: -1, Mem: -1}
		if s.State == resourceRunning && s.CPUPercentX10 >= 0 {
			p.CPU = float64(s.CPUPercentX10) / 10
		}
		if s.State == resourceRunning && s.MemoryMiB >= 0 {
			p.Mem = s.MemoryMiB
		}
		// Bandwidth = counter delta vs the previous minute, only when the
		// container kept running across both samples (same boot, counters
		// monotonically increasing).
		if prev != nil && s.State == resourceRunning && prev.State == resourceRunning &&
			s.BootTime != 0 && s.BootTime == prev.BootTime &&
			s.RXBytesTotal >= prev.RXBytesTotal && s.TXBytesTotal >= prev.TXBytesTotal {
			p.RX = s.RXBytesTotal - prev.RXBytesTotal
			p.TX = s.TXBytesTotal - prev.TXBytesTotal
		}
		out = append(out, p)
		prev = s
	}
	return out, nil
}

// resolvedState picks the persisted state, falling back to a live status for
// users that have no sample yet (e.g. created after the last background
// sample), so a brand-new container is not shown as stopped.
func resolvedState(sample db.ResourceSample, live string) string {
	if sample.SampleMinute != 0 {
		return resourceStateName(sample.State)
	}
	if live != "" {
		return live
	}
	return "-"
}

// liveStatusFallback issues ONE instances-list call, and only when some user
// lacks a persisted sample yet. In the steady state (every user sampled) it
// returns nil and the dashboard stays free of Incus calls.
func (m *Manager) liveStatusFallback(users []*db.User, latest map[int64]db.ResourceSample) map[string]string {
	for _, u := range users {
		if _, ok := latest[u.ID]; !ok {
			st, err := m.lx.InstanceStatuses()
			if err != nil {
				return nil
			}
			return st
		}
	}
	return nil
}

func decorateStoredUsage(r *Result, sample db.ResourceSample, hasSample bool, average float64, hasAverage bool) {
	if hasSample && sample.State == resourceRunning && hasAverage {
		r.CPUUse = fmt.Sprintf("%.0f%%", average)
	} else {
		r.CPUUse = "-"
	}
	if hasSample && sample.State == resourceRunning {
		r.MemUse = fmt.Sprintf("%d MiB", sample.MemoryMiB)
	} else {
		r.MemUse = "-"
	}
}

// PanelResources returns the persisted usage for a user's panel page: the
// five-minute CPU average and the latest memory/disk usage, formatted for
// display. No Incus call is made — the data comes from the background sampler.
func (m *Manager) PanelResources(userID int64) (cpuUse, memUse, diskUse string) {
	latest, err := m.db.LatestResourceSamples()
	if err != nil {
		return "-", "-", "-"
	}
	avg, err := m.db.AverageCPU(time.Now().Add(-5 * time.Minute).Unix())
	if err != nil {
		avg = map[int64]float64{}
	}
	sample, ok := latest[userID]
	if !ok || sample.State != resourceRunning {
		return "-", "-", "-"
	}
	if pct, ok := avg[userID]; ok {
		cpuUse = fmt.Sprintf("%.0f%%", pct)
	} else {
		cpuUse = "-"
	}
	memUse = humanBytes(sample.MemoryMiB * (1 << 20))
	diskUse = humanBytes(sample.DiskUsedMiB * (1 << 20))
	return cpuUse, memUse, diskUse
}
