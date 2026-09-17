package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"vpsmgr/internal/lx"
)

// fakePTY stands in for a container's shell so the session logic — the grace
// period, the replay buffer, resuming — is testable without an Incus daemon.
type fakePTY struct {
	out chan []byte

	mu     sync.Mutex
	closed bool
	writes bytes.Buffer
	resize []lx.TermSize
}

func newFakePTY() *fakePTY { return &fakePTY{out: make(chan []byte, 64)} }

func (f *fakePTY) Output() <-chan []byte { return f.out }

func (f *fakePTY) Write(b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes.Write(b)
	return nil
}

func (f *fakePTY) Resize(s lx.TermSize) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resize = append(f.resize, s)
	return nil
}

func (f *fakePTY) ExitCode() (int, error) { return 0, nil }

func (f *fakePTY) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	close(f.out)
}

func (f *fakePTY) sizes() []lx.TermSize {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]lx.TermSize(nil), f.resize...)
}

// fakeSink records what a client was sent.
type fakeSink struct {
	mu     sync.Mutex
	binary bytes.Buffer
	ctl    []termOut
	fail   bool
}

var errSinkWedged = errors.New("sink is wedged")

func (s *fakeSink) write(messageType int, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errSinkWedged
	}
	if messageType == websocket.BinaryMessage {
		s.binary.Write(data)
		return nil
	}
	var out termOut
	if err := json.Unmarshal(data, &out); err == nil {
		s.ctl = append(s.ctl, out)
	}
	return nil
}

func (s *fakeSink) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.binary.String()
}

// waitFor polls until cond is true or the budget runs out.
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

func TestTermRegistryCapAndOwnership(t *testing.T) {
	reg := newTermRegistry()
	a, err := reg.create("alice", newFakePTY())
	if err != nil {
		t.Fatal(err)
	}
	if reg.atCap("alice") {
		t.Error("one session must not be the cap")
	}
	b, err := reg.create("alice", newFakePTY())
	if err != nil {
		t.Fatal(err)
	}
	if !reg.atCap("alice") {
		t.Error("two sessions should be the cap")
	}
	if _, err := reg.create("alice", newFakePTY()); err != errTermLimit {
		t.Errorf("third create = %v, want errTermLimit", err)
	}
	if _, err := reg.create("bob", newFakePTY()); err != nil {
		t.Errorf("another account must be unaffected: %v", err)
	}
	if a.token == b.token {
		t.Error("tokens must be unique")
	}

	// A token is good for its own user and its own shell only.
	if got, ok := reg.lookup(a.token, "alice"); !ok || got != a {
		t.Error("lookup of one's own token failed")
	}
	if _, ok := reg.lookup(a.token, "bob"); ok {
		t.Error("another user's token must not resume a shell")
	}
	if _, ok := reg.lookup("nope", "alice"); ok {
		t.Error("an unknown token must not resolve")
	}
}

func TestTermRegistryTakeoverAndRemove(t *testing.T) {
	reg := newTermRegistry()
	a, _ := reg.create("alice", newFakePTY())
	b, _ := reg.create("alice", newFakePTY())

	if n := reg.others("alice", a.token); n != 1 {
		t.Errorf("others = %d, want 1", n)
	}
	if n := reg.killOthers("alice", a.token); n != 1 {
		t.Errorf("killOthers = %d, want 1", n)
	}
	// A killed session stops being resumable straight away, even though its
	// pump may still be draining.
	if _, ok := reg.lookup(b.token, "alice"); ok && b.closed {
		t.Error("a closed session must not be resumable")
	}
	reg.remove(b)
	if n := reg.others("alice", a.token); n != 0 {
		t.Errorf("others after removal = %d, want 0", n)
	}
	if _, ok := reg.lookup(b.token, "alice"); ok {
		t.Error("a removed session must not be resumable")
	}
}

// While nobody is attached the shell keeps producing output; a client that
// comes back has to receive it, or a resumed session would look frozen.
func TestTermSessionBuffersWhileDetachedAndReplays(t *testing.T) {
	reg := newTermRegistry()
	pty := newFakePTY()
	sess, err := reg.create("alice", pty)
	if err != nil {
		t.Fatal(err)
	}
	sess.run()

	// Detached from the start, so everything lands in the replay buffer.
	pty.out <- []byte("before-1 ")
	pty.out <- []byte("before-2 ")

	sink := &fakeSink{}
	sess.attach(sink, lx.TermSize{Cols: 80, Rows: 24})
	if !waitFor(t, func() bool { return strings.Contains(sink.text(), "before-2 ") }) {
		t.Fatalf("buffered output was not replayed: %q", sink.text())
	}

	// Live output now goes straight through.
	pty.out <- []byte("live")
	if !waitFor(t, func() bool { return strings.Contains(sink.text(), "live") }) {
		t.Fatalf("live output never arrived: %q", sink.text())
	}
	if got := sink.text(); !strings.Contains(got, "before-1 before-2 live") {
		t.Errorf("resumed stream = %q, want the buffered output then the live one", got)
	}

	// The redraw nudge: one row short, then the real size.
	sizes := pty.sizes()
	if len(sizes) < 2 || sizes[len(sizes)-1] != (lx.TermSize{Cols: 80, Rows: 24}) {
		t.Errorf("expected a SIGWINCH nudge ending at the real size, got %v", sizes)
	}
}

func TestTermSessionDetachStartsGrace(t *testing.T) {
	reg := newTermRegistry()
	sess, err := reg.create("alice", newFakePTY())
	if err != nil {
		t.Fatal(err)
	}
	sess.attach(&fakeSink{}, lx.TermSize{Cols: 80, Rows: 24})
	sess.detach()

	sess.mu.Lock()
	armed, attached := sess.expire != nil, sess.sink != nil
	sess.mu.Unlock()
	if !armed {
		t.Error("detaching should arm the grace period")
	}
	if attached {
		t.Error("detaching should clear the sink")
	}
	sess.detach() // idempotent

	// A reattach inside the window keeps the shell and disarms the timer.
	sess.attach(&fakeSink{}, lx.TermSize{Cols: 100, Rows: 30})
	sess.mu.Lock()
	armedAfter := sess.expire != nil
	sess.mu.Unlock()
	if armedAfter {
		t.Error("reattaching should disarm the grace period")
	}
	if _, ok := reg.lookup(sess.token, "alice"); !ok {
		t.Error("the resumed session must still be registered")
	}
}

func TestTermSessionReplayIsBounded(t *testing.T) {
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	sess := &termSession{pty: newFakePTY()}
	for i := 0; i < 8; i++ {
		sess.mu.Lock()
		sess.bufferLocked(chunk)
		sess.mu.Unlock()
	}
	sess.mu.Lock()
	size, over := len(sess.ring), sess.ringOver
	sess.mu.Unlock()
	if size > termReplayMax {
		t.Errorf("replay buffer = %d bytes, want <= %d", size, termReplayMax)
	}
	if !over {
		t.Error("dropping output must set the truncation notice")
	}

	sink := &fakeSink{}
	sess.attach(sink, lx.TermSize{Cols: 80, Rows: 24})
	if !strings.Contains(sink.text(), "dropped") {
		t.Error("the client must be told that output was dropped")
	}
}

// A write that fails means the client is wedged; the session has to fall back
// to buffering rather than losing the shell.
func TestTermSessionFallsBackToBufferingOnWriteFailure(t *testing.T) {
	pty := newFakePTY()
	sess := &termSession{pty: pty}
	sess.run()
	sess.attach(&fakeSink{fail: true}, lx.TermSize{Cols: 80, Rows: 24})

	pty.out <- []byte("while-wedged")
	if !waitFor(t, func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.sink == nil
	}) {
		t.Fatal("a failing sink should have detached the session")
	}
	sess.mu.Lock()
	buffered := string(sess.ring)
	sess.mu.Unlock()
	if !strings.Contains(buffered, "while-wedged") {
		t.Errorf("output was lost when the client stalled: %q", buffered)
	}
}

// Closing the shell must remove it, so a slot is never leaked.
func TestTermSessionCloseReleasesTheSlot(t *testing.T) {
	reg := newTermRegistry()
	pty := newFakePTY()
	sess, err := reg.create("alice", pty)
	if err != nil {
		t.Fatal(err)
	}
	sess.run()
	sess.attach(&fakeSink{}, lx.TermSize{Cols: 80, Rows: 24})
	sess.close()

	if !waitFor(t, func() bool { return !reg.atCap("alice") && reg.others("alice", "") == 0 }) {
		t.Error("closing a session must release its slot")
	}
	if _, ok := reg.lookup(sess.token, "alice"); ok {
		t.Error("a closed session must not be resumable")
	}
}
