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

// ResetBandwidth zeroes a user's monthly transfer totals without touching the
// Incus counter baselines (last_rx/last_tx), so the next sampler adds only the
// traffic since the reset instead of re-accumulating the pre-reset amount.
func (d *DB) ResetBandwidth(userID int64) error {
	_, err := d.sql.Exec(
		`UPDATE bandwidth SET upload_bytes=0, download_bytes=0 WHERE user_id=?`,
		userID)
	return err
}

// AllBandwidth returns the monthly totals for all users in one query. Pages
// use this instead of issuing one small lookup per container row.
func (d *DB) AllBandwidth() (map[int64]Bandwidth, error) {
	rows, err := d.sql.Query(`
		SELECT user_id, period, upload_bytes, download_bytes, last_rx, last_tx, last_pid
		FROM bandwidth`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Bandwidth{}
	for rows.Next() {
		var b Bandwidth
		if err := rows.Scan(&b.UserID, &b.Period, &b.Upload, &b.Download,
			&b.LastRX, &b.LastTX, &b.LastPID); err != nil {
			return nil, err
		}
		out[b.UserID] = b
	}
	return out, rows.Err()
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
