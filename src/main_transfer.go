package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/mgr"
	"vpsmgr/internal/ver"
)

// cmdTransfer moves one container's root disk from this host to another vpsmgr
// host (or receives one). It is a root-only command with no panel entry: it
// publishes a container's disk on the network, which is not something a panel
// request should ever be able to trigger.
//
//	send <user>        export the stopped container, serve it, and print the
//	                   command the other host runs
//	receive <url> <user>
//	                   fetch the archive, verify it, and replace that user's
//	                   container with it
//
// Only the disk travels. Domains, SSH keys, quotas, expiry and history stay on
// the source host; the receiving host re-homes the container onto its own
// network and quota.
func cmdTransfer(args []string) error {
	if len(args) < 1 {
		return errTransferUsage
	}
	switch args[0] {
	case "send":
		return transferSend(args[1:])
	case "receive":
		return transferReceive(args[1:])
	default:
		return errTransferUsage
	}
}

// transferFormat decides how an export is stored. There are two supported
// ways and no others, so that a transfer cannot be quietly three-quarters
// optimized:
//
//   - the default is the storage-driver native stream (zfs send), written as
//     the pool stores it — nothing is decompressed and recompressed, because
//     the pool has already compressed those blocks and a second pass would
//     cost CPU to save almost nothing;
//   - --portable is a tar of the files, zstd-compressed: slower to produce and
//     larger, but any pool driver can restore it and an older receiving build
//     understands it.
func transferFormat(optimized, portable bool) (bool, string, string, error) {
	if optimized && portable {
		return false, "", "", errors.New("--optimized and --portable are two different transfers — pick one")
	}
	if portable {
		return false, "zstd", "tar.zst", nil
	}
	return true, "none", "tar", nil
}

var errTransferUsage = errors.New(`usage:
  vps transfer send <user> [--portable] [--idle 5m] [--port N]
  vps transfer receive <url> <user>

An export is stored one of two ways:

  default      the storage-driver native stream (zfs send), written as the pool
               stores it: nothing is decompressed and recompressed. Fastest and
               cheapest to produce, but only a host running the same pool driver
               can restore it.
  --portable   a tar of the files, zstd-compressed. Slower to produce, but any
               pool driver can restore it and an older receiving build
               understands it.

The container must be stopped before a send, and is left stopped afterwards.
Receiving creates the account, so <user> must not exist on the receiving host
yet. Both halves must run as root, and the archive is written to (and read
from) the directory the command is started in.`)

// transferActor is what the audit log records as the operator.
func transferActor() string {
	if u := os.Getenv("SUDO_USER"); u != "" {
		return u
	}
	return "root"
}

// requireRoot guards the transfer commands. Nothing else in this CLI checks:
// those commands fail naturally when they cannot read /etc/vpsmgr. A transfer
// must not, because its job is to publish a container's disk on the network and
// to write the archive next to the operator's shell.
func requireRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("must run as root")
	}
	return nil
}

func transferManager() (*cfg.Config, *mgr.Manager, func(), error) {
	c, err := cfg.Load()
	if err != nil {
		return nil, nil, nil, err
	}
	d, err := db.Open(c.Panel.DB)
	if err != nil {
		return nil, nil, nil, err
	}
	return c, mgr.New(c, d), func() { d.Close() }, nil
}

func transferSend(args []string) error {
	if len(args) < 1 {
		return errTransferUsage
	}
	name := strings.ToLower(args[0])
	fs := flag.NewFlagSet("transfer send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var optimized, portable bool
	var idle time.Duration
	var port int
	// --optimized is the default; it stays accepted so commands written
	// against 1.10.x keep working.
	fs.BoolVar(&optimized, "optimized", false, "")
	fs.BoolVar(&portable, "portable", false, "")
	fs.DurationVar(&idle, "idle", 5*time.Minute, "")
	fs.IntVar(&port, "port", 0, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	if err := mgr.ValidateExistingName(name); err != nil {
		return err
	}
	optimized, compression, ext, err := transferFormat(optimized, portable)
	if err != nil {
		return err
	}
	what := "a portable zstd tar"
	if optimized {
		what = "an optimized storage-driver stream"
	}

	c, m, closeDB, err := transferManager()
	if err != nil {
		return err
	}
	defer closeDB()

	// Handle the signals from the very start, not just while the listener is
	// up: the export is the longest part of a send, and an operator who
	// interrupts it must still get their working directory back clean. The
	// context is what unwinds the in-flight Incus request.
	ctx, cancel := interruptible()
	defer cancel()

	// The archive lands in the directory the operator started the command in,
	// so it is written where they can see and account for it — never /tmp,
	// which is tmpfs on most hosts and would spend RAM on it.
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	sweepTransferArchives(dir)
	estimate, err := m.TransferEstimate(name)
	if err != nil {
		return err
	}
	free, err := freeBytes(dir)
	if err != nil {
		return err
	}
	if estimate > free {
		return fmt.Errorf("%s has %s free but the container holds about %s — free some space, or run the command from a larger filesystem",
			dir, mgr.HumanBytes(free), mgr.HumanBytes(estimate))
	}

	path, err := transferArchivePath(dir, ext)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// The archive is a copy of a user's whole disk: root-only while it exists,
	// and removed on every exit path.
	defer func() {
		f.Close()
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fmt.Printf("  ! warn: could not remove %s: %v\n", path, err)
		}
	}()

	fmt.Printf("exporting as %s (up to %s)…\n", what, mgr.HumanBytes(estimate))
	counter := &countingWriter{w: f}
	stop := printTicker(time.Second, func() {
		fmt.Printf("\r\033[K  exporting… %s", mgr.HumanBytes(counter.n.Load()))
	})
	// Refuse to write past the space that was free when the check ran, so a
	// mis-estimate cannot fill the filesystem.
	guard := &spaceGuard{limit: free - free/20}
	manifest, err := m.TransferExport(ctx, name, io.MultiWriter(counter, guard),
		mgr.TransferOptions{Optimized: optimized, Compression: compression, Actor: transferActor()})
	stop()
	fmt.Print("\r\033[K")
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("interrupted — no archive was left behind")
		}
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("vpsmgr %s exported %s in %s\n", ver.Version, mgr.HumanBytes(manifest.Bytes), manifest.Elapsed.Round(time.Second))

	// The account metadata goes next to the archive and is served from the same
	// listener, so the receiving host can read it — and check it has room —
	// before pulling the disk down.
	metaPath, err := transferArchivePath(dir, "json")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
			fmt.Printf("  ! warn: could not remove %s: %v\n", metaPath, err)
		}
	}()
	if err := os.WriteFile(metaPath, manifest.Meta, 0o600); err != nil {
		return err
	}

	host := c.DisplayIP()
	srv, err := mgr.NewTransferServer(mgr.TransferFiles{
		Archive:     path,
		ArchiveSHA:  manifest.SHA256,
		Meta:        metaPath,
		MetaSHA:     manifest.MetaSHA,
		Compression: manifest.Compression,
		Optimized:   manifest.Optimized,
		Version:     ver.Version,
		Driver:      manifest.Driver,
	}, host, port)
	if err != nil {
		return fmt.Errorf("%w (the address comes from panel.display_ip, falling back to panel.public_ip — it is what the other host will dial)", err)
	}
	defer srv.Close()

	acct, _, _ := mgr.TransferMetaAccount(manifest.Meta)
	restore := "any pool driver can restore it"
	if optimized {
		restore = "the other host must run the same pool driver"
	}
	fmt.Printf("\nsha256 %s\n", manifest.SHA256)
	fmt.Printf("on the other machine, run:\n\n")
	fmt.Printf("  vps transfer receive '%s' <user>\n\n", srv.URL())
	fmt.Printf("<user> must not exist there yet. It brings %s, keys, notes and init script,\n",
		mgr.MachineSpecs(acct.CPU, acct.MemMB, acct.DiskGB))
	fmt.Printf("and needs about %s of room; %s.\n", mgr.HumanBytes(manifest.DiskUsed), restore)
	fmt.Printf("listening on port %d up to %s — Ctrl-C removes the archive.\n", srv.Port(), idle)

	served := make(chan error, 1)
	go func() { served <- srv.Serve(idle, transferTick(idle)) }()

	select {
	case <-ctx.Done():
		fmt.Print("\r\033[K")
		fmt.Println("interrupted — closing the listener and removing the archive")
		srv.Close()
		return nil
	case err := <-served:
		fmt.Print("\r\033[K")
		if err != nil {
			return err
		}
		fmt.Printf("nobody connected for %s — closing the listener and removing the archive\n", idle)
		return nil
	}
}

// interruptible returns a context that is cancelled by SIGINT, SIGTERM or
// SIGHUP (the last one is what an SSH session that goes away sends). Both
// halves of a transfer run under it so that interrupting the command unwinds
// the request in flight and lets the deferred cleanup run — leaving no archive,
// no half-written download and no stray listener behind.
func interruptible() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(sigs)
	}()
	return ctx, cancel
}

// transferTick prints one line a second while the listener waits. The output is
// not just cosmetic: it is what keeps an idle SSH session (or a NAT mapping)
// from being dropped during a long transfer.
func transferTick(idle time.Duration) func(mgr.TransferStats) {
	return func(st mgr.TransferStats) {
		if st.Active == 0 {
			fmt.Printf("\r\033[K  waiting for the other host… stops in %s", st.IdleLeft.Round(time.Second))
			return
		}
		fmt.Printf("\r\033[K  connected %s — %s sent, %s/s",
			st.Connected.Round(time.Second), mgr.HumanBytes(st.Served), mgr.HumanBytes(st.BytesPerSecond))
	}
}

func transferReceive(args []string) error {
	if len(args) < 2 {
		return errTransferUsage
	}
	rawURL := args[0]
	name := strings.ToLower(args[1])
	fs := flag.NewFlagSet("transfer receive", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	link, err := parseTransferURL(rawURL)
	if err != nil {
		return err
	}

	_, m, closeDB, err := transferManager()
	if err != nil {
		return err
	}
	defer closeDB()

	ctx, cancel := interruptible()
	defer cancel()

	// Fail on a bad target, or on a host this feature does not cover, now
	// rather than after a long download.
	if err := m.TransferTarget(name); err != nil {
		return err
	}
	if err := m.TransferHostSupported(); err != nil {
		return err
	}
	// An optimized archive is storage-driver specific and the stream says
	// nothing about itself, so the driver travels in the URL and is compared
	// here — before the metadata fetch, let alone a multi-gigabyte download.
	if link.optimized {
		mine, err := m.PoolDriver()
		if err != nil {
			return err
		}
		if err := checkOptimizedCompat(link, mine); err != nil {
			return err
		}
	}

	// The account metadata is tiny and comes first: it says what will be
	// created and how much room the disk needs, so a host that cannot take the
	// container can say so before pulling gigabytes through the wire.
	fmt.Printf("fetching from %s\n", link.target)
	meta, err := fetchMeta(ctx, link.metaURL, link.pin, link.wantMeta)
	if err != nil {
		return err
	}
	if err := m.TransferPrecheck(meta); err != nil {
		return err
	}
	acct, from, err := mgr.TransferMetaAccount(meta)
	if err != nil {
		return err
	}
	origin := ""
	if from != "" && from != name {
		origin = fmt.Sprintf(" (it was %q there)", from)
	}
	fmt.Printf("creating %s as %s, with this host's own IP, ports and IPv6%s\n",
		name, mgr.MachineSpecs(acct.CPU, acct.MemMB, acct.DiskGB), origin)

	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	sweepTransferArchives(dir)
	path, err := transferArchivePath(dir, "part")
	if err != nil {
		return err
	}
	_ = os.Remove(path)
	defer func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fmt.Printf("  ! warn: could not remove %s: %v\n", path, err)
		}
	}()

	if err := fetchArchive(ctx, link.target, path, link.pin, link.wantSHA); err != nil {
		return err
	}
	fmt.Printf("checksum verified — %s\n", link.describe())

	archive, err := os.Open(path)
	if err != nil {
		return err
	}
	fmt.Printf("importing into %s (this creates the account)…\n", name)
	res, err := m.TransferImport(ctx, name, archive, meta, mgr.TransferOptions{Actor: transferActor()})
	archive.Close()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted — %s was removed again; nothing was left behind", name)
		}
		return err
	}

	fmt.Printf("\n%s is up after %s, with %s of data from the other host\n",
		res.User, res.Elapsed.Round(time.Second), mgr.HumanBytes(res.BytesRead))
	fmt.Printf("  ip:        %s\n", res.IP)
	fmt.Printf("  ssh:       ssh -p %d root@%s\n", res.SSHPort, res.IP)
	fmt.Printf("  ports:     %s\n", res.Ports)
	fmt.Printf("  root pw:   unchanged (it came with the disk)\n")
	if res.PanelPass != "" {
		fmt.Printf("  panel pw:  %s  (shown once)\n", res.PanelPass)
	}
	fmt.Printf("\nquota, keys, notes and init script came across; the source host's domains and\n")
	fmt.Printf("snapshots did not — point the DNS here and add the domains before retiring it.\n")
	return nil
}

// transferURL is a parsed receive command: where to fetch the two files, what
// to verify them against, and what the sending host reported about the archive
// it produced.
type transferURL struct {
	target      string
	metaURL     string
	wantSHA     string
	wantMeta    string
	pin         string
	compression string
	version     string
	driver      string
	optimized   bool
}

// describe names the archive and the build that produced it, so the operator
// can see what is about to be imported before it starts.
func (u transferURL) describe() string {
	what := "a portable tar archive"
	switch {
	case u.optimized:
		what = "an --optimized storage-driver stream (" + u.driver + ")"
	case u.compression != "":
		what = "a tar archive with " + u.compression + " compression"
	}
	if u.version == "" {
		return "exported by an older build, as " + what
	}
	return "exported by vpsmgr " + u.version + ", as " + what
}

// checkOptimizedCompat refuses an --optimized archive this host cannot restore.
// Such a stream is storage-driver specific — a zfs send stream cannot be
// unpacked into a btrfs or dir pool — and it says nothing about itself, so the
// sending host's driver travels in the URL and is compared here, before a
// single byte is fetched.
// When the sending host said which driver it used and it differs, that is
// certain knowledge and the transfer is refused. When it said nothing — an
// older sender, or a hand-written URL — the stream is given the benefit of the
// doubt and left to Incus, which reports the mismatch itself if there is one.
func checkOptimizedCompat(u transferURL, myDriver string) error {
	if !u.optimized || u.driver == "" {
		return nil
	}
	if u.driver != myDriver && u.driver != "" {
		return fmt.Errorf("this host's pool runs %s but the archive is an --optimized stream from a %s pool, and such a stream can only be imported into a %s pool — re-export on the sending host without --optimized",
			myDriver, u.driver, u.driver)
	}
	return nil
}

// parseTransferURL splits the receive URL into what to fetch and what to verify
// against. Everything after the '#' is for this side only: HTTP clients never
// send a fragment, so the certificate pin, both checksums and the description
// of the archive stay on the operator's command line, and the listener never
// learns them.
//
// compression, optimized, driver and v are additions: a command printed by an
// older build has none of them (the zero values mean "not stated"), and one
// from a newer build carries extras that an older receiver ignores, so the
// halves keep working across versions.
func parseTransferURL(raw string) (transferURL, error) {
	var out transferURL
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return out, err
	}
	if u.Scheme != "https" {
		return out, fmt.Errorf("refusing %q: a transfer must be https", raw)
	}
	if u.Host == "" || !strings.HasPrefix(u.Path, "/d/") {
		return out, errors.New("this is not a vps transfer URL")
	}
	frag, err := url.ParseQuery(u.Fragment)
	if err != nil {
		return out, err
	}
	out.wantSHA, out.wantMeta, out.pin = frag.Get("sha256"), frag.Get("meta"), frag.Get("cert")
	out.compression, out.version, out.driver = frag.Get("compression"), frag.Get("v"), frag.Get("driver")
	out.optimized = frag.Get("optimized") == "1"
	if !isHexDigest(out.wantSHA) || !isHexDigest(out.wantMeta) || !isHexDigest(out.pin) {
		return out, errors.New("the URL is missing its #sha256=…&meta=…&cert=… part — copy the whole command from the sending host")
	}
	u.Fragment, u.RawFragment = "", ""
	// The account metadata is served from the same token on its own path.
	m := *u
	m.Path = strings.Replace(u.Path, "/d/", "/m/", 1)
	out.target, out.metaURL = u.String(), m.String()
	return out, nil
}

// fetchMeta downloads the small account metadata into memory and checks it
// against the digest that came with the command.
func fetchMeta(ctx context.Context, url, pin, wantSHA string) ([]byte, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify:    true,
			VerifyPeerCertificate: pinVerifier(pin),
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the sending host answered %s for the account details", resp.Status)
	}
	// The metadata is small by construction (a quota, some keys, notes); a cap
	// keeps a wrong URL from buffering something enormous.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTransferMetaBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxTransferMetaBytes {
		return nil, errors.New("the account details are implausibly large")
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != wantSHA {
		return nil, fmt.Errorf("the account details did not survive the trip (got %s, expected %s)", got, wantSHA)
	}
	return body, nil
}

// maxTransferMetaBytes bounds the account metadata. The items it carries are
// each capped by the panel (init script, sticky notes), so this is generous
// headroom rather than a limit anyone should meet.
const maxTransferMetaBytes = 1 << 20

func isHexDigest(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// fetchArchive downloads target into path, resuming from a partial file left by
// an earlier attempt, and finally checks the whole file against wantSHA.
//
// The peer's certificate is checked against pin. InsecureSkipVerify only turns
// off the chain check — a self-signed peer cannot satisfy one — and
// VerifyPeerCertificate puts a stronger check in its place: an exact match on
// the certificate whose fingerprint came with the command.
func fetchArchive(ctx context.Context, target, path, pin, wantSHA string) error {
	// Only the connection setup is bounded: the body of a multi-gigabyte
	// transfer legitimately takes as long as it takes, and a stalled one is
	// caught by the retry loop instead of a total deadline.
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	client := &http.Client{Transport: &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify:    true,
			VerifyPeerCertificate: pinVerifier(pin),
		},
	}}
	// Read-write, not write-only: the finished file is read back to checksum it.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	var written, total atomic.Int64
	if st, err := f.Stat(); err == nil {
		written.Store(st.Size())
	}
	rate := &rateMeter{}
	stop := printTicker(time.Second, func() {
		progress := ""
		if t := total.Load(); t > 0 {
			progress = fmt.Sprintf(" (%d%%)", written.Load()*100/t)
		}
		fmt.Printf("\r\033[K  downloading%s — %s at %s/s",
			progress, mgr.HumanBytes(written.Load()), mgr.HumanBytes(rate.rate(written.Load())))
	})

	var have int64
	if st, err := f.Stat(); err == nil {
		have = st.Size()
	}
	var lastErr error
	for attempt := 1; attempt <= transferMaxAttempts; attempt++ {
		done, err := fetchOnce(ctx, client, target, f, have, &written, &total)
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err
		if done || ctx.Err() != nil {
			break
		}
		delay := transferRetryDelay(attempt)
		fmt.Printf("\r\033[K  interrupted (%v) — retrying in %s\n", err, delay)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
		if st, statErr := f.Stat(); statErr == nil {
			have = st.Size()
		}
	}
	stop()
	fmt.Print("\r\033[K")
	if lastErr != nil {
		return lastErr
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != wantSHA {
		return fmt.Errorf("checksum mismatch: the archive did not survive the trip (got %s, expected %s)", got, wantSHA)
	}
	return nil
}

// transferMaxAttempts bounds the download retries. A transfer can be resumed
// where it stopped, so a flaky link only costs the bytes in flight.
const transferMaxAttempts = 10

func transferRetryDelay(attempt int) time.Duration {
	d := time.Duration(attempt) * 2 * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// fetchOnce performs one download attempt, appending to f from offset have. It
// reports done=true when the failure is not worth retrying (a rejected pin, a
// 4xx, a range the server will not satisfy).
func fetchOnce(ctx context.Context, client *http.Client, target string, f *os.File, have int64, written, total *atomic.Int64) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return true, err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := client.Do(req)
	if err != nil {
		// A rejected certificate will be rejected every time: retrying only
		// delays telling the operator they are talking to the wrong host.
		if errors.Is(err, errPinMismatch) || ctx.Err() != nil {
			return true, err
		}
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		// The server sent the whole file: whatever we had is void.
		if have > 0 {
			if err := f.Truncate(0); err != nil {
				return true, err
			}
			written.Store(0)
			have = 0
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return true, err
		}
	case http.StatusPartialContent:
		if _, err := f.Seek(have, io.SeekStart); err != nil {
			return true, err
		}
	case http.StatusRequestedRangeNotSatisfiable:
		// The file is already complete.
		return true, nil
	default:
		return true, fmt.Errorf("the sending host answered %s", resp.Status)
	}
	if resp.ContentLength > 0 {
		total.Store(have + resp.ContentLength)
	}
	if _, err := io.Copy(io.MultiWriter(f, &counterSink{n: written}), resp.Body); err != nil {
		return ctx.Err() != nil, err
	}
	return true, nil
}

// errPinMismatch marks a certificate that is not the one the receive command
// pinned. It is a sentinel because the failure is permanent: the download loop
// must report it instead of retrying it.
var errPinMismatch = errors.New("certificate pin mismatch")

// pinVerifier checks the peer's leaf certificate against the expected SHA-256.
// A self-signed peer cannot be validated by a chain, so the pin is the whole
// trust decision — and a mismatch has to abort before any bytes are read.
func pinVerifier(pin string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("%w: the peer presented no certificate", errPinMismatch)
		}
		sum := sha256.Sum256(rawCerts[0])
		if hex.EncodeToString(sum[:]) != pin {
			return fmt.Errorf("%w: the host at the other end is not the one that printed this command", errPinMismatch)
		}
		return nil
	}
}

// transferArchivePath names the archive after the owning process and a random
// UUID. The PID is what lets a later run recognise an archive whose owner was
// killed outright — SIGKILL, or the machine going down — because that is the
// one exit no cleanup code can run on. The UUID keeps two transfers started in
// one directory apart.
func transferArchivePath(dir, ext string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s/vps-transfer-%d-%x-%x-%x-%x-%x.%s",
		dir, os.Getpid(), b[0:4], b[4:6], b[6:8], b[8:10], b[10:16], ext), nil
}

// sweepTransferArchives removes transfer files that a previous run left in dir
// after being killed without a chance to clean up. The archive is a copy of a
// user's whole disk, so leaving one lying around is not acceptable.
//
// A file whose PID still belongs to a running process is somebody's live
// transfer and is left alone. If a PID has been reused by an unrelated process
// the file is kept too — the sweep errs towards keeping, never towards deleting
// something in use.
func sweepTransferArchives(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), "vps-transfer-")
		if !ok {
			continue
		}
		dash := strings.IndexByte(rest, '-')
		if dash <= 0 {
			continue
		}
		pid, err := strconv.Atoi(rest[:dash])
		if err != nil || processAlive(pid) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := os.Remove(path); err == nil {
			fmt.Printf("removed %s — left behind by a transfer that was killed\n", path)
		}
	}
}

// processAlive reports whether pid names a process this host can signal. EPERM
// means it exists but belongs to someone else, which still counts as alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// freeBytes reports the space available in dir.
func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// countingWriter counts the bytes written through it.
type countingWriter struct {
	w io.Writer
	n atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// counterSink adds the bytes copied through it to a shared counter.
type counterSink struct{ n *atomic.Int64 }

func (c *counterSink) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return len(p), nil
}

// spaceGuard fails the export if it would write past the space that was free
// when the check ran, so a mis-estimate can never fill the filesystem.
type spaceGuard struct {
	limit int64
	seen  int64
}

func (g *spaceGuard) Write(p []byte) (int, error) {
	g.seen += int64(len(p))
	if g.seen > g.limit {
		return 0, errors.New("the export ran past the free space that was available when it started")
	}
	return len(p), nil
}

// rateMeter turns a growing byte counter into a per-second rate. Only the
// ticker goroutine calls rate, so it needs no locking.
type rateMeter struct {
	at  time.Time
	val int64
}

func (m *rateMeter) rate(cur int64) int64 {
	now := time.Now()
	var perSecond int64
	if !m.at.IsZero() {
		if secs := now.Sub(m.at).Seconds(); secs > 0 {
			perSecond = int64(float64(cur-m.val) / secs)
		}
	}
	m.at, m.val = now, cur
	if perSecond < 0 {
		return 0
	}
	return perSecond
}

// printTicker runs fn every d until the returned stop function is called.
func printTicker(d time.Duration, fn func()) func() {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				fn()
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}
