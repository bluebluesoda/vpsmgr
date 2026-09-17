package lx

import (
	"encoding/json"
	"strings"
	"testing"
)

// The resize message is the one protocol detail that is easy to get wrong:
// Incus reads the size from Args as strings, and silently ignores width/height
// at the top level. This pins the shape.
func TestExecControlResize(t *testing.T) {
	b, err := json.Marshal(execControl{
		Command: "window-resize",
		Args:    map[string]string{"width": "120", "height": "40"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["command"] != "window-resize" {
		t.Errorf("command = %v", got["command"])
	}
	args, ok := got["args"].(map[string]any)
	if !ok {
		t.Fatalf("args is not an object: %s", b)
	}
	if args["width"] != "120" || args["height"] != "40" {
		t.Errorf("args = %v, want string width/height", args)
	}
	if _, bad := got["width"]; bad {
		t.Errorf("width must not be a top-level field: %s", b)
	}
}

func TestTermSizeNormalized(t *testing.T) {
	cases := []struct{ in, want TermSize }{
		{TermSize{Cols: 120, Rows: 40}, TermSize{Cols: 120, Rows: 40}},
		{TermSize{}, TermSize{Cols: 80, Rows: 24}},
		{TermSize{Cols: -5, Rows: 0}, TermSize{Cols: 80, Rows: 24}},
		{TermSize{Cols: 5000, Rows: 5000}, TermSize{Cols: 80, Rows: 24}},
	}
	for _, tc := range cases {
		if got := tc.in.normalized(); got != tc.want {
			t.Errorf("%+v normalized to %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

// An interactive exec does not end when the shell exits, so the wrapper prints
// an OSC marker and the session ends when it is seen. The marker can be split
// across reads, which is the case worth testing.
func TestPtyExitMarker(t *testing.T) {
	const marker = "\x1b]777;vpsmgr-exit="
	cases := []struct {
		name     string
		chunks   []string
		wantDone bool
		wantCode int
	}{
		{"marker in one chunk", []string{"output" + marker + "0\a"}, true, 0},
		{"split across chunks", []string{"out" + marker[:6], marker[6:] + "7\a"}, true, 7},
		{"split before the digits", []string{marker, "13\a"}, true, 13},
		{"no marker", []string{"just ordinary output"}, false, -1},
		{"marker without a status terminator", []string{marker + "9"}, false, -1},
	}
	for _, tc := range cases {
		p := &Pty{out: make(chan []byte, 8), done: make(chan struct{}), code: -1}
		for _, chunk := range tc.chunks {
			p.scanExit([]byte(chunk))
		}
		select {
		case <-p.done:
			if !tc.wantDone {
				t.Errorf("%s: session ended, want it alive", tc.name)
			}
		default:
			if tc.wantDone {
				t.Errorf("%s: session still alive, want it ended", tc.name)
			}
		}
		if p.code != tc.wantCode {
			t.Errorf("%s: exit code = %d, want %d", tc.name, p.code, tc.wantCode)
		}
	}
}

// The wrapper must keep the login shell's status and end the pty afterwards.
func TestExitMarkerCmdShape(t *testing.T) {
	if !strings.Contains(exitMarkerCmd, "/bin/bash -l") {
		t.Errorf("wrapper does not run a login shell: %q", exitMarkerCmd)
	}
	if !strings.Contains(exitMarkerCmd, "\\033]777;vpsmgr-exit=%d\\007") {
		t.Errorf("wrapper does not print the exit marker: %q", exitMarkerCmd)
	}
	if !strings.Contains(exitMarkerCmd, "$?") {
		t.Errorf("wrapper does not pass the shell's status: %q", exitMarkerCmd)
	}
}
