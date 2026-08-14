package db

// Bandwidth holds a user's monthly transfer totals and the last observed Incus
// network counters used as the accumulation baseline.
type Bandwidth struct {
	UserID   int64
	Period   string
	Upload   uint64 // bytes, this period
	Download uint64 // bytes, this period
	LastRX   int64  // last observed cumulative download counter
	LastTX   int64  // last observed cumulative upload counter
	LastPID  int64  // init PID of the container at the last sample
}

func (d *DB) GetBandwidth(userID int64) (*Bandwidth, error) {
	t := &Bandwidth{}
	err := d.sql.QueryRow(
		`SELECT user_id, period, upload_bytes, download_bytes, last_rx, last_tx, last_pid
		 FROM bandwidth WHERE user_id=?`, userID).
		Scan(&t.UserID, &t.Period, &t.Upload, &t.Download, &t.LastRX, &t.LastTX, &t.LastPID)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ApplyBandwidth advances a user's monthly totals from the current Incus counters
// rx/tx (and the container's init PID at sample time). The delta is computed in
// a single atomic SQL statement against the stored baselines, handling four
// cases at once:
//
//   - normal: same process instance, counter grew -> add the difference
//   - out-of-order: same PID but counter shrank -> a LATE OLD SAMPLE from a
//     concurrent sampler (panel daemon + CLI). The counter was already
//     advanced by the newer sample, so nothing is added; only the baseline is
//     refreshed. Without the PID check this was mistaken for a container
//     restart and double-counted (review P2-3).
//   - restart/reinstall: PID changed (or first sample ever) -> the counters
//     genuinely reset to 0 and grew again, so add the post-reset value.
//   - rollover: period key changed (new month) -> zero the totals first
//
// Because the delta uses the baseline present in the database at statement
// time, concurrent samplers never double-count.
func (d *DB) ApplyBandwidth(userID int64, period string, rx, tx, pid int64) error {
	_, err := d.sql.Exec(`
		INSERT INTO bandwidth(user_id, period, upload_bytes, download_bytes, last_rx, last_tx, last_pid)
		VALUES(?, ?, 0, 0, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			download_bytes = (CASE WHEN bandwidth.period <> excluded.period THEN 0 ELSE download_bytes END) +
			                 (CASE
			                   WHEN excluded.last_rx >= bandwidth.last_rx
			                     THEN excluded.last_rx - bandwidth.last_rx
			                   WHEN excluded.last_pid <> 0 AND excluded.last_pid <> bandwidth.last_pid
			                     THEN excluded.last_rx
			                   ELSE 0 END),
			upload_bytes = (CASE WHEN bandwidth.period <> excluded.period THEN 0 ELSE upload_bytes END) +
			               (CASE
			                 WHEN excluded.last_tx >= bandwidth.last_tx
			                   THEN excluded.last_tx - bandwidth.last_tx
			                 WHEN excluded.last_pid <> 0 AND excluded.last_pid <> bandwidth.last_pid
			                   THEN excluded.last_tx
			                 ELSE 0 END),
			last_rx = excluded.last_rx,
			last_tx = excluded.last_tx,
			last_pid = excluded.last_pid,
			period = excluded.period`,
		userID, period, rx, tx, pid)
	return err
}
