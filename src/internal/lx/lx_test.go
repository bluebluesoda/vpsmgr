package lx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHasShebang(t *testing.T) {
	cases := []struct {
		script string
		want   bool
	}{
		{"#!/bin/bash\napt-get update", true},
		{"  #!/usr/bin/env bash\nx", true},
		{"#!/bin/sh\nx", true},
		{"echo hi", false},
		{"", false},
		{"\n#!\n", false}, // first line is blank, not a shebang
		{"# not a shebang\nx", false},
	}
	for _, c := range cases {
		if got := hasShebang(c.script); got != c.want {
			t.Errorf("hasShebang(%q) = %v, want %v", c.script, got, c.want)
		}
	}
}

// TestInitScriptCmdStructure guards the shell-scoping fix: the delivery
// (cat+chmod) must stay in the foreground and only the run may be backgrounded
// inside a subshell — a trailing `&` would background the whole chain, so sh -c
// exits before cat reads stdin and the file is left empty.
func TestInitScriptCmdStructure(t *testing.T) {
	cmd := initScriptCmd("#!/bin/sh\necho hi\n", "/root/vpsmgr-init.sh", "/var/log/vpsmgr-init.log")
	if !strings.HasPrefix(cmd, "cat >/root/vpsmgr-init.sh && chmod 700 /root/vpsmgr-init.sh && (") {
		t.Errorf("delivery must be foreground, got: %s", cmd)
	}
	if !strings.Contains(cmd, "&)") {
		t.Errorf("run must be backgrounded inside a subshell, got: %s", cmd)
	}
	if strings.Contains(cmd, "&& nohup ") {
		t.Errorf("run must not be chained with && (would background the whole chain), got: %s", cmd)
	}

	noShebang := initScriptCmd("echo hi\n", "/root/vpsmgr-init.sh", "/var/log/vpsmgr-init.log")
	if !strings.Contains(noShebang, "nohup sh /root/vpsmgr-init.sh") {
		t.Errorf("no-shebang run must use 'nohup sh <path>', got: %s", noShebang)
	}
	if strings.Contains(noShebang, "nohup /root/vpsmgr-init.sh") {
		t.Errorf("no-shebang run must not execute the file directly, got: %s", noShebang)
	}
}

// TestInitScriptCmdRuns tests the real sh -c semantics end to end in a temp
// dir: the script content must be written in FULL synchronously (regression:
// the old trailing-`&` form left an empty file), sh -c must return promptly,
// and the run must be detached (the marker appears after the command returns).
func TestInitScriptCmdRuns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vpsmgr-init.sh")
	logPath := filepath.Join(dir, "vpsmgr-init.log")
	marker := filepath.Join(dir, "marker")
	script := "#!/bin/sh\necho ran >" + marker + "\n"

	cmd := exec.Command("/bin/sh", "-c", initScriptCmd(script, path, logPath))
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sh -c failed: %v (%s)", err, out)
	}

	// The script must be written in full synchronously — the old bug left it
	// empty because cat was backgrounded and killed before writing.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("init script not written: %v", err)
	}
	if string(b) != script {
		t.Errorf("init script content = %q, want %q", b, script)
	}

	// The run is detached: it must eventually execute without blocking sh -c.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detached script never ran")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestInitScriptCmdRunsNoShebang covers the no-shebang path (run under sh).
func TestInitScriptCmdRunsNoShebang(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vpsmgr-init.sh")
	logPath := filepath.Join(dir, "vpsmgr-init.log")
	marker := filepath.Join(dir, "marker")
	script := "echo ran >" + marker + "\n"

	cmd := exec.Command("/bin/sh", "-c", initScriptCmd(script, path, logPath))
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sh -c failed: %v (%s)", err, out)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("init script not written: %v", err)
	}
	if string(b) != script {
		t.Errorf("init script content = %q, want %q", b, script)
	}
}
