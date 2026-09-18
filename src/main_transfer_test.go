package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vpsmgr/internal/mgr"
)

// startTransfer writes content to a file and serves it, returning the receive
// command and the pin/checksum it carries. The listener is closed on cleanup.
func startTransfer(t *testing.T, content []byte) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "archive.tar.gz")
	if err := os.WriteFile(file, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	srv, err := mgr.NewTransferServer(file, "127.0.0.1", 0, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("NewTransferServer: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	go func() { _ = srv.Serve(time.Hour, nil) }()

	target, wantSHA, pin, err := parseTransferURL(srv.URL())
	if err != nil {
		t.Fatalf("parseTransferURL(%q): %v", srv.URL(), err)
	}
	if sum := sha256.Sum256(content); wantSHA != hex.EncodeToString(sum[:]) {
		t.Fatalf("URL checksum = %q, want the file's sha256", wantSHA)
	}
	der := srv.Fingerprint()
	if pin != der {
		t.Fatalf("URL pin = %q, want the listener's fingerprint %q", pin, der)
	}
	return target, wantSHA, pin
}

// TestTransferRoundTrip covers the whole receive path against a real listener:
// certificate pinning, the download, and the checksum.
func TestTransferRoundTrip(t *testing.T) {
	content := bytes.Repeat([]byte("vpsmgr transfer payload "), 64*1024)
	target, wantSHA, pin := startTransfer(t, content)

	dest := filepath.Join(t.TempDir(), "got.part")
	if err := fetchArchive(t.Context(), target, dest, pin, wantSHA); err != nil {
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
	target, wantSHA, pin := startTransfer(t, content)

	dest := filepath.Join(t.TempDir(), "got.part")
	if err := os.WriteFile(dest, content[:len(content)/3], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fetchArchive(t.Context(), target, dest, pin, wantSHA); err != nil {
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
// certificate must be refused before the transfer starts.
func TestTransferRejectsWrongPin(t *testing.T) {
	content := []byte("payload")
	target, wantSHA, _ := startTransfer(t, content)

	other := sha256.Sum256([]byte("some other certificate"))
	err := fetchArchive(t.Context(), target, filepath.Join(t.TempDir(), "got.part"),
		hex.EncodeToString(other[:]), wantSHA)
	if err == nil {
		t.Fatal("a mismatch on the certificate pin was accepted")
	}
	if !strings.Contains(err.Error(), "pin") && !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("error does not mention the certificate: %v", err)
	}
}

// TestTransferRejectsWrongChecksum: a complete download that does not hash to
// the expected digest must not be handed to the importer.
func TestTransferRejectsWrongChecksum(t *testing.T) {
	content := []byte("payload")
	target, _, pin := startTransfer(t, content)

	wrong := sha256.Sum256([]byte("something else"))
	err := fetchArchive(t.Context(), target, filepath.Join(t.TempDir(), "got.part"),
		pin, hex.EncodeToString(wrong[:]))
	if err == nil {
		t.Fatal("a checksum mismatch was accepted")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("error does not mention the checksum: %v", err)
	}
}

// TestTransferUnknownToken: only the one path the sender printed is served.
func TestTransferUnknownToken(t *testing.T) {
	target, wantSHA, pin := startTransfer(t, []byte("payload"))

	u := target[:strings.Index(target, "/d/")] + "/d/00000000-0000-4000-8000-000000000000"
	err := fetchArchive(t.Context(), u, filepath.Join(t.TempDir(), "got.part"), pin, wantSHA)
	if err == nil {
		t.Fatal("a request for an unknown token was served")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("want a 404 for an unknown token, got: %v", err)
	}
}

// TestParseTransferURL: the digests must travel in the URL, and plain http is
// never acceptable.
func TestParseTransferURL(t *testing.T) {
	sum := sha256.Sum256(nil)
	digest := hex.EncodeToString(sum[:])

	for _, tc := range []struct {
		name string
		url  string
		ok   bool
	}{
		{"complete", "https://1.2.3.4:8443/d/tok#sha256=" + digest + "&cert=" + digest, true},
		{"plain http", "http://1.2.3.4:8443/d/tok#sha256=" + digest + "&cert=" + digest, false},
		{"no fragment", "https://1.2.3.4:8443/d/tok", false},
		{"no pin", "https://1.2.3.4:8443/d/tok#sha256=" + digest, false},
		{"not a digest", "https://1.2.3.4:8443/d/tok#sha256=zz&cert=" + digest, false},
		{"wrong path", "https://1.2.3.4:8443/other/tok#sha256=" + digest + "&cert=" + digest, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := parseTransferURL(tc.url)
			if tc.ok && err != nil {
				t.Fatalf("rejected a valid URL: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("accepted an invalid URL")
			}
		})
	}
}

// TestTransferServerIdleExit: with nobody connected the listener gives up on
// its own and releases the port, which is what stops a forgotten `send` from
// leaving an archive on the network indefinitely.
func TestTransferServerIdleExit(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "archive.tar")
	if err := os.WriteFile(file, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("payload"))
	srv, err := mgr.NewTransferServer(file, "127.0.0.1", 0, hex.EncodeToString(sum[:]))
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
