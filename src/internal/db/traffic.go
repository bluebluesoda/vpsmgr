package db

// Traffic holds a user's monthly transfer totals and the last observed Incus
// network counters used as the accumulation baseline.
type Traffic struct {
	UserID   int64
	Period   string
	Upload   uint64 // bytes, this period
	Download uint64 // bytes, this period
	LastRX   int64  // last observed cumulative download counter
	LastTX   int64  // last observed cumulative upload counter
}

func (d *DB) GetTraffic(userID int64) (*Traffic, error) {
	t := &Traffic{}
	err := d.sql.QueryRow(
		`SELECT user_id, period, upload_bytes, download_bytes, last_rx, last_tx
		 FROM traffic WHERE user_id=?`, userID).
		Scan(&t.UserID, &t.Period, &t.Upload, &t.Download, &t.LastRX, &t.LastTX)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ApplyTraffic advances a user's monthly totals from the current Incus counters
// rx/tx. The delta is computed in a single atomic SQL statement against the
// stored baselines, handling three cases at once:
//
//   - normal: counter grew -> add the difference, keep the new baseline
//   - reset: counter shrank (container restart/reinstall) -> add the post-reset
//     traffic only, re-baseline
//   - rollover: period key changed (new month) -> zero the totals first
//
// Because the delta uses the baseline present in the database at statement
// time, concurrent samplers never double-count.
func (d *DB) ApplyTraffic(userID int64, period string, rx, tx int64) error {
	_, err := d.sql.Exec(`
		INSERT INTO traffic(user_id, period, upload_bytes, download_bytes, last_rx, last_tx)
		VALUES(?, ?, 0, 0, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			download_bytes = (CASE WHEN traffic.period <> excluded.period THEN 0 ELSE download_bytes END) +
			                 (CASE WHEN excluded.last_rx >= traffic.last_rx
			                       THEN excluded.last_rx - traffic.last_rx
			                       ELSE excluded.last_rx END),
			upload_bytes = (CASE WHEN traffic.period <> excluded.period THEN 0 ELSE upload_bytes END) +
			               (CASE WHEN excluded.last_tx >= traffic.last_tx
			                     THEN excluded.last_tx - traffic.last_tx
			                     ELSE excluded.last_tx END),
			last_rx = excluded.last_rx,
			last_tx = excluded.last_tx,
			period = excluded.period`,
		userID, period, rx, tx)
	return err
}
