package lx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// TermSize is a terminal window size in character cells.
type TermSize struct {
	Cols int
	Rows int
}

// normalized clamps a size into what a PTY will accept and fills in the
// conventional 80x24 for anything nonsensical (a zero-sized window, a browser
// that has not laid the terminal out yet).
func (s TermSize) normalized() TermSize {
	if s.Cols < 2 || s.Cols > 1000 {
		s.Cols = 80
	}
	if s.Rows < 2 || s.Rows > 1000 {
		s.Rows = 24
	}
	return s
}

// execOpen starts an exec operation and connects every websocket fd it hands
// back. The caller owns the returned connections. All fds must be connected
// before the command runs, which is why this is shared: the fire-and-collect
// Exec and the interactive Attach need exactly these steps.
func (c *Client) execOpen(ctx context.Context, name string, req execReq) (string, map[string]*websocket.Conn, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/1.0/instances/"+url.PathEscape(name)+"/exec", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", nil, err
	}
	var op struct {
		Type      string `json:"type"`
		Operation string `json:"operation"`
		Error     string `json:"error"`
		Metadata  struct {
			Inner struct {
				Fds map[string]string `json:"fds"`
			} `json:"metadata"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &op); err != nil {
		return "", nil, fmt.Errorf("incus exec %s: bad response: %w", name, err)
	}
	if op.Type == "error" {
		return "", nil, errors.New(op.Error)
	}
	if op.Operation == "" {
		return "", nil, fmt.Errorf("incus exec %s: no operation returned", name)
	}
	if len(op.Metadata.Inner.Fds) == 0 {
		return "", nil, fmt.Errorf("incus exec %s: no websocket fds in response", name)
	}
	opID := strings.TrimPrefix(op.Operation, "/1.0/operations/")

	dialer := c.dialer()
	conns := make(map[string]*websocket.Conn, len(op.Metadata.Inner.Fds))
	for k, secret := range op.Metadata.Inner.Fds {
		wsURL := "ws://unix/1.0/operations/" + opID + "/websocket?secret=" + secret
		ws, _, err := dialer.DialContext(ctx, wsURL, nil)
		if err != nil {
			for _, open := range conns {
				_ = open.Close()
			}
			return "", nil, fmt.Errorf("incus exec %s: connect fd %s: %w", name, k, err)
		}
		conns[k] = ws
	}
	return opID, conns, nil
}

// exitMarker is an OSC sequence the wrapper shell prints once the login shell
// is gone. Incus does not end an interactive exec when the process exits — the
// operation stays "Running" and the fd stays open — so the session needs its
// own end-of-stream signal. It is an OSC (not plain text) so that a terminal
// ignores it if it ever reaches one, and Pty strips it from the stream anyway.
const exitMarker = "\x1b]777;vpsmgr-exit="

// exitMarkerCmd runs the login shell in a wrapper that reports its exit status
// on the pty and then lets the pty close.
const exitMarkerCmd = "/bin/bash -l; printf '\\033]777;vpsmgr-exit=%d\\007' $?"

// execControl is a client→server message on an exec operation's control
// channel. That channel only carries signals and window resizes; the exit
// status is read from the operation record instead.
//
// The size travels as strings inside Args, not as top-level numbers — that is
// what api.InstanceExecControl does, and sending width/height at the top level
// is silently ignored.
type execControl struct {
	Command string            `json:"command"`
	Args    map[string]string `json:"args,omitempty"`
	Signal  int               `json:"signal,omitempty"`
}

// Pty is a live interactive terminal session inside a container.
//
// Output arrives as it is produced, stdin stays writable for the life of the
// session, and the window size can change at any time — the three things Exec
// deliberately does not do. Backpressure is natural: the read goroutine blocks
// sending to Output, so a slow consumer stops draining the Incus socket instead
// of growing a buffer here.
type Pty struct {
	c     *Client
	opID  string
	conns map[string]*websocket.Conn

	out  chan []byte
	done chan struct{}
	once sync.Once
	wg   sync.WaitGroup // the read loops, so out is only closed once they stop
	tail []byte         // rolling window used to spot the exit marker
	code int            // exit status reported by the marker, -1 until then

	mu   sync.Mutex
	dead bool
}

// Attach starts a login shell on a PTY inside the container.
func (c *Client) Attach(name string, size TermSize) (*Pty, error) {
	size = size.normalized()
	req := execReq{
		Command:          []string{"/bin/sh", "-c", exitMarkerCmd},
		WaitForWebsocket: true,
		Interactive:      true,
		Environment: map[string]string{
			// A real TERM so the shell enables colours and line editing, and a
			// UTF-8 locale so non-ASCII output survives.
			"TERM":      "xterm-256color",
			"COLORTERM": "truecolor",
			"LANG":      "C.UTF-8",
			"HOME":      "/root",
		},
		User:   0,
		Group:  0,
		Width:  size.Cols,
		Height: size.Rows,
	}
	opID, conns, err := c.execOpen(context.Background(), name, req)
	if err != nil {
		return nil, err
	}
	if conns["0"] == nil {
		for _, open := range conns {
			_ = open.Close()
		}
		return nil, fmt.Errorf("incus attach %s: no stdin fd", name)
	}
	p := &Pty{c: c, opID: opID, conns: conns, out: make(chan []byte, 128), done: make(chan struct{}), code: -1}
	// An interactive exec allocates a PTY, and the server then hands back only
	// "0" (the PTY, used in BOTH directions) plus "control" — stdout and stderr
	// are folded into that one channel. Reading every fd except the control
	// channel covers that shape and the 0/1/2 shape alike.
	for key, ws := range conns {
		if key == "control" {
			continue
		}
		p.wg.Add(1)
		go p.readLoop(ws)
	}
	go p.drainControl()
	go p.monitor()
	return p, nil
}

// Output is the merged terminal stream. It is closed when the session ends.
func (p *Pty) Output() <-chan []byte { return p.out }

// readLoop forwards one fd to the output channel until it closes. On an
// interactive session this includes fd "0", which carries output as well as
// accepting input (gorilla allows one reader and one writer at a time). A read
// error is just how the session ends — the authoritative outcome is the exit
// status on the operation record, so it is not treated as a failure here.
func (p *Pty) readLoop(ws *websocket.Conn) {
	defer p.wg.Done()
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			p.finish()
			return
		}
		if len(msg) == 0 {
			continue
		}
		p.scanExit(msg)
		select {
		case p.out <- msg:
		case <-p.done:
			return
		}
	}
}

// scanExit looks for the wrapper's exit marker in the stream. The marker can
// straddle two reads, so a small rolling window is kept. The bytes themselves
// are still forwarded: an OSC sequence is invisible to the terminal.
func (p *Pty) scanExit(msg []byte) {
	p.mu.Lock()
	p.tail = append(p.tail, msg...)
	if len(p.tail) > 256 {
		p.tail = p.tail[len(p.tail)-256:]
	}
	window := string(p.tail)
	p.mu.Unlock()

	i := strings.Index(window, exitMarker)
	if i < 0 {
		return
	}
	rest := window[i+len(exitMarker):]
	if len(rest) == 0 {
		return // the status digits have not arrived yet
	}
	end := strings.IndexByte(rest, 7) // BEL terminates the OSC
	if end < 0 {
		return
	}
	if n, err := strconv.Atoi(rest[:end]); err == nil {
		p.mu.Lock()
		p.code = n
		p.mu.Unlock()
	}
	p.finish()
}

// drainControl consumes the control channel. It is client→server only, so
// anything arriving is discarded; it still has to be read, or the session can
// stall.
func (p *Pty) drainControl() {
	ws := p.conns["control"]
	if ws == nil {
		return
	}
	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			return
		}
	}
}

// finish tears the session down exactly once. The output channel is closed by
// a separate goroutine after every read loop has stopped: closing it here would
// race a read loop that is mid-send and panic on "send on closed channel".
func (p *Pty) finish() {
	p.once.Do(func() {
		p.mu.Lock()
		p.dead = true
		p.mu.Unlock()
		close(p.done)
		for _, ws := range p.conns {
			_ = ws.Close()
		}
		go func() {
			p.wg.Wait()
			close(p.out)
		}()
	})
}

// monitor waits for the operation to stop running. An interactive session does
// not signal the end by closing its websocket when the shell exits (the client
// still holds the pty open), so the operation record is what tells us the shell
// is gone.
func (p *Pty) monitor() {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-t.C:
			if !p.opRunning() {
				p.finish()
				return
			}
		}
	}
}

// opRunning reports whether the exec operation is still in progress. A failed
// lookup is not evidence that the session ended, so it counts as still running:
// only a definitive non-Running status ends the session.
func (p *Pty) opRunning() bool {
	var op struct {
		Status string `json:"status"`
	}
	if err := p.c.get("/1.0/operations/"+p.opID, &op); err != nil {
		return true
	}
	return op.Status == "Running"
}

// Write sends keystrokes to the PTY.
func (p *Pty) Write(b []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dead {
		return errors.New("terminal session is closed")
	}
	return p.conns["0"].WriteMessage(websocket.BinaryMessage, b)
}

// Resize tells the PTY its new window size so full-screen programs reflow. The
// control channel is write-only for us; a failure is reported but need not end
// the session.
func (p *Pty) Resize(size TermSize) error {
	size = size.normalized()
	ws := p.conns["control"]
	if ws == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dead {
		return nil
	}
	msg, err := json.Marshal(execControl{
		Command: "window-resize",
		Args: map[string]string{
			"width":  strconv.Itoa(size.Cols),
			"height": strconv.Itoa(size.Rows),
		},
	})
	if err != nil {
		return err
	}
	return ws.WriteMessage(websocket.TextMessage, msg)
}

// ExitCode reports the shell's exit status. It is read from the operation
// record, not from the control channel (which is client→server only), so it is
// only meaningful once the session has ended.
func (p *Pty) ExitCode() (int, error) {
	p.mu.Lock()
	if p.code >= 0 {
		code := p.code
		p.mu.Unlock()
		return code, nil
	}
	p.mu.Unlock()
	var op struct {
		Metadata struct {
			Return *int `json:"return"`
		} `json:"metadata"`
	}
	if err := p.c.get("/1.0/operations/"+p.opID, &op); err != nil {
		return -1, err
	}
	if op.Metadata.Return == nil {
		return -1, nil
	}
	return *op.Metadata.Return, nil
}

// Done is closed when the session ends.
func (p *Pty) Done() <-chan struct{} { return p.done }

// Close ends the session and releases its fds.
func (p *Pty) Close() { p.finish() }
