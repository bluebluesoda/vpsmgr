package hpx

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"vpsmgr/internal/cfg"
)

// unitPath is the shipped systemd unit relative to src/internal/hpx.
const unitPath = "../../../configs/systemd/haproxy.service"

// unitConfigArgs pulls the -f arguments out of the unit's ExecStart.
func unitConfigArgs(t *testing.T, unit string) []string {
	t.Helper()
	b, err := os.ReadFile(unit)
	if err != nil {
		t.Skipf("systemd unit not readable: %v", err)
	}
	m := regexp.MustCompile(`(?m)^ExecStart=(.*)$`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("no ExecStart in %s", unit)
	}
	var args []string
	fields := strings.Fields(m[1])
	for i := 0; i < len(fields); i++ {
		if fields[i] == "-f" && i+1 < len(fields) {
			args = append(args, fields[i+1])
			i++
		}
	}
	return args
}

// TestUnitLoadsBothConfigFiles is the regression guard for the bug that made
// every managed domain unreachable in 1.9.0/1.9.1: the unit started HAProxy
// with the STATIC entry config only, so the per-domain backends were never
// loaded. HAProxy came up healthy, listened on 80/443 and answered 404 on :80
// and closed every :443 connection — `haproxy -c` on the entry config alone
// passes, so nothing in the install path noticed.
//
// It asserts the unit passes two config files, and that the SECOND one is a
// generation file (reached through "current") which the entry config's map
// lookups depend on.
func TestUnitLoadsBothConfigFiles(t *testing.T) {
	args := unitConfigArgs(t, unitPath)
	if len(args) != 2 {
		t.Fatalf("unit ExecStart passes %d config file(s), want 2 (entry + generation): %v", len(args), args)
	}
	if args[0] != "/etc/haproxy/haproxy.cfg" {
		t.Errorf("first config = %q, want the static entry config", args[0])
	}
	if !strings.Contains(args[1], "/etc/haproxy/current/") {
		t.Errorf("second config = %q; it must be the CURRENT generation (so a reload picks up the new one)", args[1])
	}
	// The entry config's routing maps point INTO that same generation, or the
	// backends could not be found by the maps that select them.
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "configs", "haproxy", "haproxy.cfg"))
	if err != nil {
		t.Skipf("entry config not readable: %v", err)
	}
	currDir := filepath.Dir(args[1])
	for _, want := range []string{
		"map(" + currDir + "/maps/http.map)",
		"map(" + currDir + "/maps/sni.map)",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("entry config does not resolve %q — the maps must live in the generation the unit loads", want)
		}
	}
}

// TestUnitCommandLineRoutesTraffic runs the unit's OWN command line against a
// real layout and checks the routing actually works — not just that the files
// parse. Skipped without VPSMGR_TEST_HAPROXY_BIN.
func TestUnitCommandLineRoutesTraffic(t *testing.T) {
	bin := os.Getenv("VPSMGR_TEST_HAPROXY_BIN")
	if bin == "" {
		t.Skip("VPSMGR_TEST_HAPROXY_BIN not set")
	}
	args := unitConfigArgs(t, unitPath)
	if len(args) != 2 {
		t.Fatalf("unit ExecStart passes %d config files, want 2: %v", len(args), args)
	}
	if out, err := exec.Command("unshare", "-n", "true").CombinedOutput(); err != nil {
		t.Skipf("no network namespace (%v: %s)", err, strings.TrimSpace(string(out)))
	}

	// Lay out a real installed tree, then rewrite the unit's absolute paths at
	// it so the SAME arguments run against the temp copy.
	dir := t.TempDir()
	t.Setenv("VPSMGR_HAPROXY_DIR", dir)
	t.Setenv("VPSMGR_HAPROXY_BIN", bin)
	p := New(cfg.Default())
	if err := p.WriteEntryConfig(); err != nil {
		t.Fatal(err)
	}
	if err := p.PublishAll([]Domain{{Domain: "example.com", IP: "127.0.0.1"}}); err != nil {
		t.Fatal(err)
	}

	entry := strings.Replace(args[0], "/etc/haproxy", dir, 1)
	gen := strings.Replace(args[1], "/etc/haproxy", dir, 1)

	// A backend that answers on :80 so a routed request is distinguishable
	// from the reject backend's 404.
	backend := filepath.Join(dir, "backend.py")
	if err := os.WriteFile(backend, []byte(httpEchoBackend), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
set -e
ip link set lo up
python3 "$1" &
sleep 0.5
exec "$2" -f "$3" -f "$4"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	// The published config binds :80/:443 and points at the container IP, which
	// is not this host — rewrite the backend target to the local test server
	// and the listeners to unprivileged ports, INSIDE the temp generation only.
	genDomains := filepath.Join(dir, "current", "domains.cfg")
	body, err := os.ReadFile(genDomains)
	if err != nil {
		t.Fatal(err)
	}
	fixed := strings.ReplaceAll(string(body), "127.0.0.1:80", "127.0.0.1:8081")
	if err := os.WriteFile(genDomains, []byte(fixed), 0o644); err != nil {
		t.Fatal(err)
	}
	entryBody, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	entryBody = []byte(strings.ReplaceAll(strings.ReplaceAll(string(entryBody), "bind :80", "bind :18080"),
		"bind :443", "bind :18443"))
	if err := os.WriteFile(entry, entryBody, 0o644); err != nil {
		t.Fatal(err)
	}

	// --fork makes the shell (and the haproxy it execs) children of the process
	// we capture, so its network namespace is the one nsenter must enter.
	cmd := exec.Command("unshare", "-n", "--fork", "bash", script, backend, bin, entry, gen)
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatalf("start haproxy: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	probe := filepath.Join(dir, "probe.sh")
	if err := os.WriteFile(probe, []byte(`for i in $(seq 1 30); do
  curl -s -m 1 -o /dev/null http://127.0.0.1:18080/ && break
  sleep 0.1
done
curl -s -m 3 -H 'Host: example.com' http://127.0.0.1:18080/
`), 0o755); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		out, err := exec.Command("nsenter", "-t", itoa(cmd.Process.Pid), "-n", "bash", probe).CombinedOutput()
		if err == nil && strings.Contains(string(out), "ROUTED-OK") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the unit's command line did not route the host to its backend (got %q)", strings.TrimSpace(string(out)))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

const httpEchoBackend = `import http.server, socketserver
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        b = b"ROUTED-OK"
        self.send_response(200)
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)
    def log_message(self, *a): pass
socketserver.TCPServer(("127.0.0.1", 8081), H).serve_forever()
`

func itoa(n int) string { return strconv.Itoa(n) }
