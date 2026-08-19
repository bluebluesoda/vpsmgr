package db

import (
	"database/sql"
)

// ResourceSample is one compact, minute-granularity snapshot for a managed
// container. Counter fields are cumulative Incus counters; CPU is also stored
// as a derived value so dashboard queries do not need to reconstruct it.
type ResourceSample struct {
	UserID           int64
	SampleMinute     int64
	State            int
	BootTime         int64
	CPUSecondsNS     int64
	CPUPercentX10    int64
	MemoryMiB        int64
	Processes        int64
	DiskUsedMiB      int64
	RXBytesTotal     int64
	TXBytesTotal     int64
	DiskReadBytes    int64
	DiskWrittenBytes int64
}

// BandwidthObservation carries the current Incus counters used to advance the
// exact monthly bandwidth totals in the same transaction as the resource row.
type BandwidthObservation struct {
	UserID   int64
	RX       int64
	TX       int64
	BootTime int64
}

const bandwidthUpsert = `
	INSERT INTO bandwidth(user_id, period, upload_bytes, download_bytes, last_rx, last_tx, last_pid, last_boot_time)
	VALUES(?, ?, 0, 0, ?, ?, ?, ?)
	ON CONFLICT(user_id) DO UPDATE SET
		download_bytes = (CASE WHEN bandwidth.period <> excluded.period THEN 0 ELSE download_bytes END) +
		                 (CASE
		                   WHEN excluded.last_rx >= bandwidth.last_rx
		                     THEN excluded.last_rx - bandwidth.last_rx
		                   WHEN (excluded.last_boot_time <> 0 AND excluded.last_boot_time <> bandwidth.last_boot_time)
		                     OR (excluded.last_boot_time = 0 AND excluded.last_pid <> 0 AND excluded.last_pid <> bandwidth.last_pid)
		                     THEN excluded.last_rx
		                   ELSE 0 END),
		upload_bytes = (CASE WHEN bandwidth.period <> excluded.period THEN 0 ELSE upload_bytes END) +
		               (CASE
		                 WHEN excluded.last_tx >= bandwidth.last_tx
		                   THEN excluded.last_tx - bandwidth.last_tx
		                 WHEN (excluded.last_boot_time <> 0 AND excluded.last_boot_time <> bandwidth.last_boot_time)
		                   OR (excluded.last_boot_time = 0 AND excluded.last_pid <> 0 AND excluded.last_pid <> bandwidth.last_pid)
		                   THEN excluded.last_tx
		                 ELSE 0 END),
		last_rx = excluded.last_rx,
		last_tx = excluded.last_tx,
		last_pid = excluded.last_pid,
		last_boot_time = excluded.last_boot_time,
		period = excluded.period`

type sqlExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func applyBandwidth(exec sqlExecer, userID int64, period string, rx, tx, pid, bootTime int64) error {
	_, err := exec.Exec(bandwidthUpsert, userID, period, rx, tx, pid, bootTime)
	return err
}

// ApplyBandwidth preserves the standalone API used by tests and CLI paths.
// New resource sampling supplies bootTime; the pid fallback keeps old callers
// correct during the schema transition.
func (d *DB) ApplyBandwidth(userID int64, period string, rx, tx, pid int64) error {
	return applyBandwidth(d.sql, userID, period, rx, tx, pid, 0)
}

// RecordResourceSamples atomically records the current batch, advances monthly
// bandwidth totals, and removes samples older than the fixed seven-day window.
func (d *DB) RecordResourceSamples(samples []ResourceSample, observations []BandwidthObservation, period string, cutoff int64) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	rollback := func(err error) error {
		_ = tx.Rollback()
		return err
	}

	const insert = `
		INSERT INTO resource_samples(
			user_id, sample_minute, state, boot_time, cpu_seconds_ns,
			cpu_pct_x10, memory_mib, processes, disk_used_mib,
			rx_bytes_total, tx_bytes_total, disk_read_bytes_total,
			disk_write_bytes_total)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, sample_minute) DO UPDATE SET
			state=excluded.state,
			boot_time=excluded.boot_time,
			cpu_seconds_ns=excluded.cpu_seconds_ns,
			cpu_pct_x10=excluded.cpu_pct_x10,
			memory_mib=excluded.memory_mib,
			processes=excluded.processes,
			disk_used_mib=excluded.disk_used_mib,
			rx_bytes_total=excluded.rx_bytes_total,
			tx_bytes_total=excluded.tx_bytes_total,
			disk_read_bytes_total=excluded.disk_read_bytes_total,
			disk_write_bytes_total=excluded.disk_write_bytes_total`
	stmt, err := tx.Prepare(insert)
	if err != nil {
		return rollback(err)
	}
	for _, s := range samples {
		if _, err := stmt.Exec(s.UserID, s.SampleMinute, s.State, s.BootTime,
			s.CPUSecondsNS, s.CPUPercentX10, s.MemoryMiB, s.Processes,
			s.DiskUsedMiB, s.RXBytesTotal, s.TXBytesTotal,
			s.DiskReadBytes, s.DiskWrittenBytes); err != nil {
			_ = stmt.Close()
			return rollback(err)
		}
	}
	if err := stmt.Close(); err != nil {
		return rollback(err)
	}
	for _, o := range observations {
		if err := applyBandwidth(tx, o.UserID, period, o.RX, o.TX, 0, o.BootTime); err != nil {
			return rollback(err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM resource_samples WHERE sample_minute < ?`, cutoff); err != nil {
		return rollback(err)
	}
	return tx.Commit()
}

// LatestResourceSamples returns the newest sample for every user.
func (d *DB) LatestResourceSamples() (map[int64]ResourceSample, error) {
	rows, err := d.sql.Query(`
		SELECT s.user_id, s.sample_minute, s.state, s.boot_time,
		       s.cpu_seconds_ns, s.cpu_pct_x10, s.memory_mib, s.processes,
		       s.disk_used_mib, s.rx_bytes_total, s.tx_bytes_total,
		       s.disk_read_bytes_total, s.disk_write_bytes_total
		FROM resource_samples s
		JOIN (
			SELECT user_id, MAX(sample_minute) AS sample_minute
			FROM resource_samples GROUP BY user_id
		) latest ON latest.user_id=s.user_id AND latest.sample_minute=s.sample_minute`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]ResourceSample{}
	for rows.Next() {
		var s ResourceSample
		if err := rows.Scan(&s.UserID, &s.SampleMinute, &s.State, &s.BootTime,
			&s.CPUSecondsNS, &s.CPUPercentX10, &s.MemoryMiB, &s.Processes,
			&s.DiskUsedMiB, &s.RXBytesTotal, &s.TXBytesTotal,
			&s.DiskReadBytes, &s.DiskWrittenBytes); err != nil {
			return nil, err
		}
		out[s.UserID] = s
	}
	return out, rows.Err()
}

// AverageCPU returns the average stored CPU percentage (not a live sample) for
// each user since the supplied minute. Values are in ordinary percentage units.
func (d *DB) AverageCPU(since int64) (map[int64]float64, error) {
	rows, err := d.sql.Query(`
		SELECT user_id, AVG(cpu_pct_x10)
		FROM resource_samples
		WHERE state=1 AND cpu_pct_x10 >= 0 AND sample_minute >= ?
		GROUP BY user_id`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]float64{}
	for rows.Next() {
		var id int64
		var value float64
		if err := rows.Scan(&id, &value); err != nil {
			return nil, err
		}
		out[id] = value / 10
	}
	return out, rows.Err()
}

// ResourceHistory returns samples for a future resource chart.
func (d *DB) ResourceHistory(userID, since int64) ([]ResourceSample, error) {
	rows, err := d.sql.Query(`
		SELECT user_id, sample_minute, state, boot_time,
		       cpu_seconds_ns, cpu_pct_x10, memory_mib, processes,
		       disk_used_mib, rx_bytes_total, tx_bytes_total,
		       disk_read_bytes_total, disk_write_bytes_total
		FROM resource_samples
		WHERE user_id=? AND sample_minute >= ?
		ORDER BY sample_minute`, userID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ResourceSample
	for rows.Next() {
		var s ResourceSample
		if err := rows.Scan(&s.UserID, &s.SampleMinute, &s.State, &s.BootTime,
			&s.CPUSecondsNS, &s.CPUPercentX10, &s.MemoryMiB, &s.Processes,
			&s.DiskUsedMiB, &s.RXBytesTotal, &s.TXBytesTotal,
			&s.DiskReadBytes, &s.DiskWrittenBytes); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
