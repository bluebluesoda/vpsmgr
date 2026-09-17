package panel

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"vpsmgr/internal/csrf"
	"vpsmgr/internal/lx"
)

const (
	// termMaxPerUser caps how many shells one account can hold open. A terminal
	// is a live process per connection, so this is a resource guard as much as
	// a UX one.
	termMaxPerUser = 2
	// termIdleTimeout closes a session the browser stopped talking on. Pings
	// keep a live one fresh, so this only fires for a closed or backgrounded
	// tab — it is what stops abandoned shells from living forever.
	termIdleTimeout = 15 * time.Minute
	termPingEvery   = 30 * time.Second
	termWriteWait   = 10 * time.Second
	// termMaxFrame caps a single message from the browser (a paste).
	termMaxFrame = 1 << 20
)

// termJS is the browser terminal engine, embedded like the templates so the
// panel keeps shipping as a single self-contained binary.
//
//go:embed assets/term.js
var termJS []byte

// termIn is a message from the browser. Terminal output travels the other way
// as raw binary frames; only control messages are JSON.
type termIn struct {
	T    string `json:"t"` // "i" input, "r" resize
	Data string `json:"data,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// termOut is a control message to the browser.
type termOut struct {
	T     string `json:"t"` // "exit" | "error"
	Code  int    `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// termRegistry counts the open shells per user. The panel is a single process,
// so an in-memory count is the whole story.
type termRegistry struct {
	mu   sync.Mutex
	open map[string]int
}

func newTermRegistry() *termRegistry { return &termRegistry{open: map[string]int{}} }

func (t *termRegistry) acquire(user string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open[user] >= termMaxPerUser {
		return false
	}
	t.open[user]++
	return true
}

func (t *termRegistry) release(user string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open[user] <= 1 {
		delete(t.open, user)
		return
	}
	t.open[user]--
}

// termSocket serialises writes: gorilla allows one writer at a time, and both
// the output pump and the keepalive pinger write.
type termSocket struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (t *termSocket) write(messageType int, data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = t.conn.SetWriteDeadline(time.Now().Add(termWriteWait))
	return t.conn.WriteMessage(messageType, data)
}

var termUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Belt and braces: the handler checks this too, but a refused handshake
	// here never reaches the handler at all.
	CheckOrigin: func(r *http.Request) bool { return csrf.SameOrigin(r) },
}

// handleWebSSH serves the standalone terminal window.
//
// It is a page rather than a modal on purpose: the terminal is the one thing in
// the panel that wants more room, and only a real browser window can be dragged
// to any size, moved to another screen or maximised. The page remembers the
// size it was last resized to, so the next open comes back the same way.
func (s *Server) handleWebSSH(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if u == nil {
		http.Redirect(w, r, s.p("/login"), http.StatusFound)
		return
	}
	if s.isExpired(r) {
		s.redirect(w, r, s.p(""), "error: "+s.t(r, "err_account_expired"))
		return
	}
	s.render(w, r, "webssh.html", pageData{
		User:   u,
		Prefix: s.p(""),
		Title:  u.Name + " — Web SSH",
	})
}

// handleTerminal serves an interactive shell for the session's own container.
//
// The container is always taken from the session, never from the request: the
// client cannot name a container it does not own. The handshake is a GET, so it
// is the one place in the panel where the POST CSRF check does not apply — the
// same-origin check below is what stands in for it.
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !csrf.SameOrigin(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	// Expired accounts are locked out of their container; a shell is exactly
	// what that lock is meant to prevent. Answered as a plain 403 rather than
	// the usual redirect, which a WebSocket client cannot follow.
	if s.isExpired(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	conn, err := termUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has already written a response
	}
	defer conn.Close()
	sock := &termSocket{conn: conn}

	if !s.terms.acquire(u.Name) {
		sock.write(websocket.TextMessage, mustJSON(termOut{T: "error",
			Error: "too many open terminals; close one and try again"}))
		return
	}
	defer s.terms.release(u.Name)

	size := termSizeFrom(r)
	pty, err := s.mgr.AttachTerminal(u.Name, size)
	if err != nil {
		sock.write(websocket.TextMessage, mustJSON(termOut{T: "error", Error: err.Error()}))
		return
	}
	defer pty.Close()
	_ = s.db.AddAuditLog(s.auditActor(r, u.Name), "terminal.open")

	conn.SetReadLimit(termMaxFrame)
	_ = conn.SetReadDeadline(time.Now().Add(termIdleTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(termIdleTimeout))
	})

	clientGone := make(chan struct{})
	go func() {
		defer close(clientGone)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(termIdleTimeout))
			var in termIn
			if json.Unmarshal(msg, &in) != nil {
				continue
			}
			switch in.T {
			case "i":
				if err := pty.Write([]byte(in.Data)); err != nil {
					return
				}
			case "r":
				_ = pty.Resize(lx.TermSize{Cols: in.Cols, Rows: in.Rows})
			}
		}
	}()

	// Keepalive. A browser answers a ping automatically, which is what refreshes
	// the read deadline above.
	stopPing := make(chan struct{})
	defer close(stopPing)
	go func() {
		t := time.NewTicker(termPingEvery)
		defer t.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-clientGone:
				return
			case <-t.C:
				if err := sock.write(websocket.PingMessage, nil); err != nil {
					return
				}
			}
		}
	}()

	// Output pump. It runs until the PTY's output channel closes, which happens
	// only after every reader has stopped — so the last bytes the shell wrote
	// are always delivered, even though the session is already over.
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		for chunk := range pty.Output() {
			if err := sock.write(websocket.BinaryMessage, chunk); err != nil {
				return
			}
		}
	}()

	select {
	case <-clientGone:
	case <-outDone:
		code, _ := pty.ExitCode()
		_ = sock.write(websocket.TextMessage, mustJSON(termOut{T: "exit", Code: code}))
		// Give the browser a moment to read the final frame before the socket
		// is torn down under it.
		time.Sleep(150 * time.Millisecond)
	}
}

// termSizeFrom reads the size the browser measured. Values are clamped by the
// lx layer, so anything nonsensical degrades to 80x24.
func termSizeFrom(r *http.Request) lx.TermSize {
	q := r.URL.Query()
	cols, _ := strconv.Atoi(q.Get("cols"))
	rows, _ := strconv.Atoi(q.Get("rows"))
	return lx.TermSize{Cols: cols, Rows: rows}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"t":"error","error":"internal error"}`)
	}
	return b
}

// termAssetPath is the URL the terminal engine is served from.
const termAssetPath = "/assets/term.js"

// handleTermAsset serves the browser terminal engine. It is embedded in the
// binary like the templates, and it is served from the panel prefix so it is
// same-origin (the panel's CSP allows no third-party scripts).
func (s *Server) handleTermAsset(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(termJS)
}
