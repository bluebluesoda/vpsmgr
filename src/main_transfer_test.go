package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vpsmgr/internal/mgr"
)

// transferFixture is a running listener and the digests its URL carries.
type transferFixture struct {
	target     string
	metaURL    string
	archiveSHA string
	metaSHA    string
	pin        string
}

// startTransfer writes an archive and the account metadata that travels with
// it, and serves both the way `vps transfer send` does.
func startTransfer(t *testing.T, content []byte) transferFixture {
	t.Helper()
	dir := t.TempDir()
	archive := filepath.Join(dir, "archive.tar.gz")
	if err := os.WriteFile(archive, content, 0o600); err != nil {
		t.Fatal(err)
	}
	meta := []byte(`{"user":"alice","cpu":2,"mem_mb":1024,"disk_gb":8,"disk_used":123,` +
		`"expires_at":"2030-01-02T15:04:05Z","ssh_keys":[{"name":"laptop","key":"ssh-ed25519 AAAA","active":true}]}`)
	metaPath := filepath.Join(dir, "meta.json")
	if err := os.WriteFile(metaPath, meta, 0o600); err != nil {
		t.Fatal(err)
	}

	archiveSum := sha256.Sum256(content)
	metaSum := sha256.Sum256(meta)
	srv, err := mgr.NewTransferServer(mgr.TransferFiles{
		Archive:    archive,
		ArchiveSHA: hex.EncodeToString(archiveSum[:]),
		Meta:       metaPath,
		MetaSHA:    hex.EncodeToString(metaSum[:]),
	}, "127.0.0.1", 0)
	if err != nil {
		t.Fatalf("NewTransferServer: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	go func() { _ = srv.Serve(time.Hour, nil) }()

	link, err := parseTransferURL(srv.URL())
	if err != nil {
		t.Fatalf("parseTransferURL(%q): %v", srv.URL(), err)
	}
	if link.wantSHA != hex.EncodeToString(archiveSum[:]) {
		t.Fatalf("URL archive checksum = %q, want the file's sha256", link.wantSHA)
	}
	if link.wantMeta != hex.EncodeToString(metaSum[:]) {
		t.Fatalf("URL metadata checksum = %q, want the file's sha256", link.wantMeta)
	}
	if !strings.Contains(link.metaURL, "/m/") {
		t.Fatalf("metadata URL %q is not on the metadata path", link.metaURL)
	}
	if link.pin != srv.Fingerprint() {
		t.Fatalf("URL pin = %q, want the listener's fingerprint %q", link.pin, srv.Fingerprint())
	}
	return transferFixture{target: link.target, metaURL: link.metaURL, archiveSHA: link.wantSHA, metaSHA: link.wantMeta, pin: link.pin}
}

// TestTransferRoundTrip covers the whole receive path against a real listener:
// certificate pinning, the account metadata, the download and the checksum.
func TestTransferRoundTrip(t *testing.T) {
	content := bytes.Repeat([]byte("vpsmgr transfer payload "), 64*1024)
	f := startTransfer(t, content)

	meta, err := fetchMeta(t.Context(), f.metaURL, f.pin, f.metaSHA)
	if err != nil {
		t.Fatalf("fetchMeta: %v", err)
	}
	var parsed mgr.TransferMeta
	if err := json.Unmarshal(meta, &parsed); err != nil {
		t.Fatalf("metadata is not readable: %v", err)
	}
	if parsed.User != "alice" || parsed.DiskGB != 8 || len(parsed.SSHKeys) != 1 {
		t.Fatalf("metadata did not survive the trip: %+v", parsed)
	}

	dest := filepath.Join(t.TempDir(), "got.part")
	if err := fetchArchive(t.Context(), f.target, dest, f.pin, f.archiveSHA); err != nil {
		t.Fatalf("fetchArchive: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("downloaded %d bytes, want %d identical bytes", len(got), len(content))
	}
}

// TestTransferResume checks that an interrupted download picks up where it
// stopped: a partly written destination must be completed, not restarted, and
// the checksum must still cover the whole file.
func TestTransferResume(t *testing.T) {
	content := bytes.Repeat([]byte("resumed payload "), 64*1024)
	f := startTransfer(t, content)

	dest := filepath.Join(t.TempDir(), "got.part")
	if err := os.WriteFile(dest, content[:len(content)/3], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fetchArchive(t.Context(), f.target, dest, f.pin, f.archiveSHA); err != nil {
		t.Fatalf("fetchArchive: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("resumed download produced %d bytes, want %d identical bytes", len(got), len(content))
	}
}

// TestTransferRejectsWrongPin: a peer that does not present the pinned
// certificate must be refused before anything is trusted, on both files.
func TestTransferRejectsWrongPin(t *testing.T) {
	f := startTransfer(t, []byte("payload"))
	other := sha256.Sum256([]byte("some other certificate"))
	pin := hex.EncodeToString(other[:])

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"metadata", func() error {
			_, err := fetchMeta(t.Context(), f.metaURL, pin, f.metaSHA)
			return err
		}},
		{"archive", func() error {
			return fetchArchive(t.Context(), f.target, filepath.Join(t.TempDir(), "got.part"), pin, f.archiveSHA)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("a mismatch on the certificate pin was accepted")
			}
			if !strings.Contains(err.Error(), "pin") && !strings.Contains(err.Error(), "certificate") {
				t.Fatalf("error does not mention the certificate: %v", err)
			}
		})
	}
}

// TestTransferRejectsWrongChecksum: neither file may be handed on when it does
// not hash to the digest that came with the command. The metadata matters as
// much as the disk: it carries the account's quota and deadline.
func TestTransferRejectsWrongChecksum(t *testing.T) {
	f := startTransfer(t, []byte("payload"))
	wrong := sha256.Sum256([]byte("something else"))
	digest := hex.EncodeToString(wrong[:])

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"metadata", func() error {
			_, err := fetchMeta(t.Context(), f.metaURL, f.pin, digest)
			return err
		}},
		{"archive", func() error {
			return fetchArchive(t.Context(), f.target, filepath.Join(t.TempDir(), "got.part"), f.pin, digest)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("a checksum mismatch was accepted")
			}
			if !strings.Contains(err.Error(), "did not survive") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestTransferUnknownToken: only the one path the sender printed is served.
func TestTransferUnknownToken(t *testing.T) {
	f := startTransfer(t, []byte("payload"))

	u := f.target[:strings.Index(f.target, "/d/")] + "/d/00000000-0000-4000-8000-000000000000"
	err := fetchArchive(t.Context(), u, filepath.Join(t.TempDir(), "got.part"), f.pin, f.archiveSHA)
	if err == nil {
		t.Fatal("a request for an unknown token was served")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("want a 404 for an unknown token, got: %v", err)
	}
}

// TestParseTransferURL: all three digests must travel in the URL, and plain
// http is never acceptable.
func TestParseTransferURL(t *testing.T) {
	sum := sha256.Sum256(nil)
	d := hex.EncodeToString(sum[:])
	frag := "#sha256=" + d + "&meta=" + d + "&cert=" + d

	for _, tc := range []struct {
		name string
		url  string
		ok   bool
	}{
		{"complete", "https://1.2.3.4:8443/d/tok" + frag, true},
		{"plain http", "http://1.2.3.4:8443/d/tok" + frag, false},
		{"no fragment", "https://1.2.3.4:8443/d/tok", false},
		{"no pin", "https://1.2.3.4:8443/d/tok#sha256=" + d + "&meta=" + d, false},
		{"no metadata digest", "https://1.2.3.4:8443/d/tok#sha256=" + d + "&cert=" + d, false},
		{"not a digest", "https://1.2.3.4:8443/d/tok#sha256=zz&meta=" + d + "&cert=" + d, false},
		{"wrong path", "https://1.2.3.4:8443/other/tok" + frag, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseTransferURL(tc.url)
			if tc.ok && err != nil {
				t.Fatalf("rejected a valid URL: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("accepted an invalid URL")
			}
		})
	}
}

// TestTransferMetaRoundTrip: what the sending host puts into the metadata is
// what the receiving host reads back out of it, including the deadline — an
// account must not gain or lose time by moving.
func TestTransferMetaRoundTrip(t *testing.T) {
	meta := mgr.TransferMeta{
		User:        "alice",
		CPU:         20,
		MemMB:       1024,
		DiskGB:      8,
		BandwidthGB: 100,
		ExpiresAt:   "2030-01-02T15:04:05Z",
		DiskUsed:    400 << 20,
		InitScript:  "apt-get update",
		SSHKeys:     []mgr.MetaKey{{Name: "laptop", Key: "ssh-ed25519 AAAA", Active: true}},
		StickyNotes: "envelope",
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	acct, user, err := mgr.TransferMetaAccount(raw)
	if err != nil {
		t.Fatalf("TransferMetaAccount: %v", err)
	}
	if user != "alice" || acct.CPU != 20 || acct.MemMB != 1024 || acct.DiskGB != 8 || acct.BandwidthGB != 100 {
		t.Fatalf("account did not survive the round trip: %+v (%s)", acct, user)
	}

	var back mgr.TransferMeta
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.ExpiresAt != meta.ExpiresAt || back.InitScript != meta.InitScript ||
		back.StickyNotes != meta.StickyNotes || len(back.SSHKeys) != 1 {
		t.Fatalf("the account's own data did not survive the round trip: %+v", back)
	}
}

// TestTransferServerIdleExit: with nobody connected the listener gives up on
// its own and releases the port, which is what stops a forgotten `send` from
// leaving an archive on the network indefinitely.
func TestTransferServerIdleExit(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "archive.tar")
	if err := os.WriteFile(archive, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(dir, "meta.json")
	if err := os.WriteFile(meta, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := mgr.NewTransferServer(mgr.TransferFiles{
		Archive: archive, ArchiveSHA: "00", Meta: meta, MetaSHA: "00",
	}, "127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	done := make(chan error, 1)
	go func() { done <- srv.Serve(1500*time.Millisecond, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the listener did not stop after its idle budget")
	}
}

// TestSpaceGuardStopsRunawayExport: the export must fail rather than fill the
// filesystem when it turns out larger than estimated.
func TestSpaceGuardStopsRunawayExport(t *testing.T) {
	g := &spaceGuard{limit: 8}
	if _, err := g.Write(make([]byte, 8)); err != nil {
		t.Fatalf("write at the limit was refused: %v", err)
	}
	if _, err := g.Write(make([]byte, 1)); err == nil {
		t.Fatal("a write past the limit was accepted")
	}
}

// TestSweepTransferArchives: a file left by a process that was killed outright
// must be picked up by the next run, while a live transfer's files and
// unrelated ones are left alone.
func TestSweepTransferArchives(t *testing.T) {
	dir := t.TempDir()

	// A reaped child's PID is free, so a file naming it looks abandoned.
	probe := exec.Command("true")
	if err := probe.Run(); err != nil {
		t.Fatalf("spawning a probe process: %v", err)
	}
	stale := filepath.Join(dir, fmt.Sprintf("vps-transfer-%d-aaaa-bbbb.cc", probe.Process.Pid))
	live := filepath.Join(dir, fmt.Sprintf("vps-transfer-%d-cccc-dddd.cc", os.Getpid()))
	other := filepath.Join(dir, "notes.txt")
	for _, p := range []string{stale, live, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	sweepTransferArchives(dir)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a file left by a killed transfer was not swept")
	}
	if _, err := os.Stat(live); err != nil {
		t.Error("a running transfer's file was swept")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("a file that is not a transfer file was swept")
	}
}

// TestTransferArchivePathIsUnique keeps two concurrent transfers started in one
// directory from writing over each other.
func TestTransferArchivePathIsUnique(t *testing.T) {
	dir := t.TempDir()
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		p, err := transferArchivePath(dir, "tar.gz")
		if err != nil {
			t.Fatal(err)
		}
		if seen[p] {
			t.Fatalf("transferArchivePath repeated %s", p)
		}
		seen[p] = true
		if filepath.Dir(p) != dir {
			t.Fatalf("archive %s is not in %s", p, dir)
		}
	}
}

// The archive's format and the sending build are additions to the URL
// fragment. A command from an older build carries neither, and one from a
// newer build carries extras an older receiver ignores, so both directions
// have to keep parsing.
func TestTransferURLFormatFields(t *testing.T) {
	d := strings.Repeat("a", sha256.Size*2)
	base := "https://1.2.3.4:8443/d/tok#sha256=" + d + "&meta=" + d + "&cert=" + d

	old, err := parseTransferURL(base)
	if err != nil {
		t.Fatalf("an older build's URL was rejected: %v", err)
	}
	if old.optimized || old.compression != "" || old.version != "" || old.driver != "" {
		t.Errorf("a URL without the new fields described itself as %+v", old)
	}

	modern, err := parseTransferURL(base + "&compression=zstd&v=1.10.2&driver=zfs")
	if err != nil {
		t.Fatal(err)
	}
	if modern.compression != "zstd" || modern.version != "1.10.2" || modern.driver != "zfs" || modern.optimized {
		t.Errorf("parsed %+v, want zstd/1.10.2/zfs and not optimized", modern)
	}

	opt, err := parseTransferURL(base + "&optimized=1&driver=btrfs")
	if err != nil {
		t.Fatal(err)
	}
	if !opt.optimized || opt.driver != "btrfs" || opt.compression != "" {
		t.Errorf("parsed %+v, want optimized on btrfs with no compression", opt)
	}
}

// An --optimized stream may only be imported into a pool running the driver it
// was produced on; everything else is refused before anything is fetched.
func TestOptimizedCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  transferURL
		mine string
		ok   bool
	}{
		{"same driver", transferURL{optimized: true, driver: "zfs"}, "zfs", true},
		{"different driver", transferURL{optimized: true, driver: "zfs"}, "btrfs", false},
		{"driver not stated", transferURL{optimized: true}, "zfs", false},
		{"plain tar ignores the driver", transferURL{compression: "zstd"}, "btrfs", true},
		{"plain tar without a driver", transferURL{}, "zfs", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkOptimizedCompat(tc.url, tc.mine)
			if tc.ok && err != nil {
				t.Fatalf("refused a compatible stream: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("accepted a stream this host cannot restore")
			}
		})
	}
}
