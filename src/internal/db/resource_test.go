package db

import "testing"

func TestRecordResourceSamplesAndRetention(t *testing.T) {
	d, err := Open(t.TempDir() + "/resource.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	makeSample := func(minute, cpu int64) ResourceSample {
		return ResourceSample{UserID: u.ID, SampleMinute: minute, State: 1,
			BootTime: 100, CPUSecondsNS: minute * 1e9, CPUPercentX10: cpu,
			MemoryMiB: 128, DiskUsedMiB: 20, RXBytesTotal: minute * 100,
			TXBytesTotal: minute * 200}
	}
	if err := d.RecordResourceSamples([]ResourceSample{makeSample(0, -1)}, []BandwidthObservation{{UserID: u.ID, RX: 100, TX: 200, BootTime: 100}}, "2026-08", -60); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordResourceSamples([]ResourceSample{makeSample(60, 100)}, []BandwidthObservation{{UserID: u.ID, RX: 150, TX: 260, BootTime: 100}}, "2026-08", 0); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordResourceSamples([]ResourceSample{makeSample(120, 200)}, []BandwidthObservation{{UserID: u.ID, RX: 10, TX: 20, BootTime: 200}}, "2026-08", 60); err != nil {
		t.Fatal(err)
	}
	latest, err := d.LatestResourceSamples()
	if err != nil || latest[u.ID].SampleMinute != 120 {
		t.Fatalf("latest sample = %+v, err=%v", latest[u.ID], err)
	}
	var up, down uint64
	tr, err := d.GetBandwidth(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	up, down = tr.Upload, tr.Download
	if up != 80 || down != 60 {
		t.Fatalf("bandwidth = up %d/down %d, want up 80/down 60", up, down)
	}
	avg, err := d.AverageCPU(0)
	if err != nil || avg[u.ID] != 15 {
		t.Fatalf("average CPU = %v, err=%v, want 15", avg[u.ID], err)
	}
	history, err := d.ResourceHistory(u.ID, 0)
	if err != nil || len(history) != 2 || history[0].SampleMinute != 60 {
		t.Fatalf("history = %+v, err=%v", history, err)
	}
}
