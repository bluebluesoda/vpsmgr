package panel

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"vpsmgr/internal/csrf"
	"vpsmgr/internal/lx"
)

const (
	// termMaxPerUser caps how many shells one account may hold, counting the
	// ones sitting in their reconnect grace period. A terminal is a live
	// process per connection, so this is a resource guard as much as a UX one.
	termMaxPerUser = 2

	// Liveness. A browser answers a WebSocket ping from its network stack,
	// without JavaScript, so this works even in a throttled background tab.
	// The interval has to sit well under the deadline: the deadline is what
	// notices a peer that vanished (a dropped NAT mapping can otherwise stay
	// silent for hours), and every message and every pong refreshes it.
	termPingEvery = 20 * time.Second
	termReadWait  = 60 * time.Second
	termWriteWait = 10 * time.Second

	// termGrace is how long a shell outlives the client that opened it. A
	// connection from far away drops for all sorts of reasons — a tunnel, a
	// cell handover, a sleeping laptop — and losing a running build or an open
	// editor to a three-second blip is the worst part of a web terminal. Two
	// minutes covers those without leaving an unattended root shell much longer.
	termGrace = 120 * time.Second

	// termReplayMax bounds what a returning client is caught up with; anything
	// older is dropped and the reader is told.
	termReplayMax = 256 << 10

	// termHelloWait bounds the first frame, before any shell exists.
	termHelloWait = 10 * time.Second
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
	T     string `json:"t"` // hello | input | resize | takeover | cancel | bye
	Token string `json:"token,omitempty"`
	Data  string `json:"data,omitempty"`
	Cols  int    `json:"cols,omitempty"`
	Rows  int    `json:"rows,omitempty"`
}

func (t termIn) size() lx.TermSize { return lx.TermSize{Cols: t.Cols, Rows: t.Rows} }

// termOut is a control message to the browser.
type termOut struct {
	T      string `json:"t"` // ready | others | busy | ping | exit | error
	Token  string `json:"token,omitempty"`
	Code   int    `json:"code,omitempty"`
	Others int    `json:"others,omitempty"`
	Error  string `json:"error,omitempty"`
}

// termPTY is the part of lx.Pty a session uses. It is an interface so the
// session's own logic — the grace period, the replay buffer, resuming — can be
// exercised without a container behind it.
type termPTY interface {
	Output() <-chan []byte
	Write([]byte) error
	Resize(lx.TermSize) error
	ExitCode() (int, error)
	Close()
}

// termSink is where a session's output goes while a client is attached.
type termSink interface {
	write(messageType int, data []byte) error
}

// termSocket serialises writes: gorilla allows one writer at a time, and the
// output pump, the keepalive pinger and the control replies all write.
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

// termSession is one shell. It outlives the WebSocket that opened it: when the
// client goes away the PTY keeps running for termGrace with its output landing
// in a bounded buffer, so a client that comes back resumes the same shell
// rather than starting a new one.
type termSession struct {
	token string
	user  string
	pty   termPTY
	reg   *termRegistry

	mu       sync.Mutex
	sink     termSink // nil while detached
	ring     []byte
	ringOver bool
	expire   *time.Timer
	closed   bool
}

// attach hands the session to a client and catches it up on what the shell
// produced while nobody was listening.
func (s *termSession) attach(sink termSink, size lx.TermSize) {
	s.mu.Lock()
	if s.expire != nil {
		s.expire.Stop()
		s.expire = nil
	}
	s.sink = sink
	replay, dropped := s.ring, s.ringOver
	s.ring, s.ringOver = nil, false
	s.mu.Unlock()

	if dropped {
		_ = sink.write(websocket.BinaryMessage,
			[]byte("\r\n\x1b[33m[ output was dropped while you were disconnected ]\x1b[0m\r\n"))
	}
	if len(replay) > 0 {
		_ = sink.write(websocket.BinaryMessage, replay)
	}
	// A returning terminal is blank, so anything painting a full screen has to
	// be told to paint again. Nudging the size by one row is what makes the
	// kernel raise SIGWINCH; the second message restores it.
	_ = s.pty.Resize(lx.TermSize{Cols: size.Cols, Rows: size.Rows - 1})
	_ = s.pty.Resize(size)
}

// detach takes the client away and starts the clock on the shell's grace
// period. It is idempotent: the reader loop and a failed write both land here.
func (s *termSession) detach() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.sink == nil {
		return
	}
	s.sink = nil
	if s.expire == nil {
		s.expire = time.AfterFunc(termGrace, s.close)
	}
}

// run starts the output pump. It belongs to the session, not to a connection,
// which is the whole point: it is still there once the client is gone.
func (s *termSession) run() {
	go func() {
		for chunk := range s.pty.Output() {
			s.mu.Lock()
			sink := s.sink
			if sink == nil {
				s.bufferLocked(chunk)
				s.mu.Unlock()
				continue
			}
			s.mu.Unlock()
			// Written outside the lock so a slow client cannot block a reattach.
			if err := sink.write(websocket.BinaryMessage, chunk); err != nil {
				// The client never received this chunk, so keep it before
				// falling back to buffering: otherwise a client that drops
				// mid-write loses the last thing the shell printed.
				s.mu.Lock()
				s.bufferLocked(chunk)
				s.mu.Unlock()
				s.detach()
			}
		}
		s.finish()
	}()
}

func (s *termSession) bufferLocked(chunk []byte) {
	s.ring = append(s.ring, chunk...)
	if len(s.ring) > termReplayMax {
		s.ring = s.ring[len(s.ring)-termReplayMax:]
		s.ringOver = true
	}
}

// close ends the shell now, whatever the grace timer was waiting for.
func (s *termSession) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.expire != nil {
		s.expire.Stop()
		s.expire = nil
	}
	s.mu.Unlock()
	// Unregister immediately: until the pump drains, a resume must not be able
	// to attach to a shell that is already on its way out.
	s.reg.remove(s)
	s.pty.Close() // the pump drains and then calls finish
}

// finish runs once the shell is gone: tell whoever is attached, then let go.
func (s *termSession) finish() {
	s.mu.Lock()
	s.closed = true
	sink := s.sink
	s.sink = nil
	if s.expire != nil {
		s.expire.Stop()
		s.expire = nil
	}
	s.mu.Unlock()

	if sink != nil {
		code, _ := s.pty.ExitCode()
		_ = sink.write(websocket.TextMessage, mustJSON(termOut{T: "exit", Code: code}))
	}
	s.reg.remove(s)
}

// termRegistry owns every live shell. Sessions are keyed by a token handed to
// the client, which is what lets a reconnect claim back its own shell.
type termRegistry struct {
	mu       sync.Mutex
	sessions map[string]*termSession
	byUser   map[string]map[string]bool
}

func newTermRegistry() *termRegistry {
	return &termRegistry{sessions: map[string]*termSession{}, byUser: map[string]map[string]bool{}}
}

var errTermLimit = errors.New("too many open terminals")

// create registers a new shell. It refuses once the user is at the cap; the
// caller decides whether to offer a takeover.
func (r *termRegistry) create(user string, pty termPTY) (*termSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.byUser[user]) >= termMaxPerUser {
		return nil, errTermLimit
	}
	token, err := newTermToken()
	if err != nil {
		return nil, err
	}
	s := &termSession{token: token, user: user, pty: pty, reg: r}
	r.sessions[token] = s
	if r.byUser[user] == nil {
		r.byUser[user] = map[string]bool{}
	}
	r.byUser[user][token] = true
	return s, nil
}

// lookup finds a session by token. The owner check is not decoration: a token
// is only ever handed to one user, and someone else's token must never resume
// their shell.
func (r *termRegistry) lookup(token, user string) (*termSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[token]
	if !ok || s.user != user {
		return nil, false
	}
	s.mu.Lock()
	closing := s.closed
	s.mu.Unlock()
	if closing {
		return nil, false
	}
	return s, true
}

// others counts the user's sessions other than except.
func (r *termRegistry) others(user, except string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for token := range r.byUser[user] {
		if token != except {
			n++
		}
	}
	return n
}

func (r *termRegistry) atCap(user string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byUser[user]) >= termMaxPerUser
}

// killOthers ends the user's other shells — how a window takes back the slots
// held by windows the user can no longer see.
func (r *termRegistry) killOthers(user, except string) int {
	r.mu.Lock()
	var doomed []*termSession
	for token := range r.byUser[user] {
		if token != except {
			if s, ok := r.sessions[token]; ok {
				doomed = append(doomed, s)
			}
		}
	}
	r.mu.Unlock()
	for _, s := range doomed {
		s.close()
	}
	return len(doomed)
}

func (r *termRegistry) remove(s *termSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, s.token)
	if set := r.byUser[s.user]; set != nil {
		delete(set, s.token)
		if len(set) == 0 {
			delete(r.byUser, s.user)
		}
	}
}

// attached returns the user's live sessions, used to push a fresh "others"
// count to each of them.
func (r *termRegistry) attached(user string) []*termSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*termSession, 0, len(r.byUser[user]))
	for token := range r.byUser[user] {
		if s, ok := r.sessions[token]; ok {
			out = append(out, s)
		}
	}
	return out
}

func newTermToken() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
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
	if !s.cfg.Panel.WebSSH {
		featureless404(w)
		return
	}
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
	// The operator switch is a real gate, not just a hidden button: with the
	// terminal disabled nothing here may start or interact with a shell, even
	// for someone replaying the endpoint by hand.
	if !s.cfg.Panel.WebSSH {
		featureless404(w)
		return
	}
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
	conn.SetReadLimit(termMaxFrame)

	// The size arrives in the first frame, before anything is created, so a
	// stray connection cannot spawn a shell by accident.
	_ = conn.SetReadDeadline(time.Now().Add(termHelloWait))
	hello, err := readTermMessage(conn)
	if err != nil {
		return
	}
	size := hello.size()
	if size.Cols == 0 || size.Rows == 0 {
		size = lx.TermSize{Cols: 80, Rows: 24}
	}

	// A returning client owns its shell: resuming is not a new session, so the
	// per-user cap must not turn it away.
	if hello.Token != "" {
		if sess, ok := s.terms.lookup(hello.Token, u.Name); ok {
			sess.attach(sock, size)
			_ = s.db.AddAuditLog(s.auditActor(r, u.Name), "terminal.resume")
			// The resuming client still has to be told it is connected: without
			// this it never learns the attempt succeeded, keeps the backoff
			// counter where it was, and shows "reconnecting" over a live shell.
			_ = sock.write(websocket.TextMessage, mustJSON(termOut{
				T: "ready", Token: sess.token, Others: s.terms.others(u.Name, sess.token),
			}))
			s.notifyOthers(u.Name)
			s.serveTermClient(conn, sock, sess, u.Name)
			return
		}
	}

	// At the cap the client is offered its slots back rather than being left
	// with a ghost it cannot see: a closed tab, or one still in its grace
	// period.
	if s.terms.atCap(u.Name) {
		_ = sock.write(websocket.TextMessage, mustJSON(termOut{
			T: "busy", Others: s.terms.others(u.Name, ""),
		}))
		reply, err := readTermMessage(conn)
		if err != nil || reply.T != "takeover" {
			return
		}
		if n := s.terms.killOthers(u.Name, ""); n > 0 {
			_ = s.db.AddAuditLog(s.auditActor(r, u.Name), "terminal.takeover")
		}
	}

	pty, err := s.mgr.AttachTerminal(u.Name, size)
	if err != nil {
		_ = sock.write(websocket.TextMessage, mustJSON(termOut{T: "error", Error: err.Error()}))
		return
	}
	sess, err := s.terms.create(u.Name, pty)
	if err != nil {
		pty.Close()
		_ = sock.write(websocket.TextMessage, mustJSON(termOut{T: "error", Error: err.Error()}))
		return
	}
	_ = s.db.AddAuditLog(s.auditActor(r, u.Name), "terminal.open")
	sess.run()
	sess.attach(sock, size)
	_ = sock.write(websocket.TextMessage, mustJSON(termOut{
		T: "ready", Token: sess.token, Others: s.terms.others(u.Name, sess.token),
	}))
	s.notifyOthers(u.Name)
	s.serveTermClient(conn, sock, sess, u.Name)
}

// serveTermClient runs the connection until the client goes away, then leaves
// the shell in its grace period instead of killing it.
func (s *Server) serveTermClient(conn *websocket.Conn, sock *termSocket, sess *termSession, user string) {
	defer sess.detach()
	defer s.notifyOthers(user)

	_ = conn.SetReadDeadline(time.Now().Add(termReadWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(termReadWait))
	})

	stopPing := make(chan struct{})
	defer close(stopPing)
	go func() {
		t := time.NewTicker(termPingEvery)
		defer t.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-t.C:
				if err := sock.write(websocket.PingMessage, nil); err != nil {
					return
				}
				// A browser cannot see protocol-level pings, so the page needs
				// its own heartbeat to notice a server that went quiet.
				_ = sock.write(websocket.TextMessage, mustJSON(termOut{T: "ping"}))
			}
		}
	}()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(termReadWait))
		in, err := readTermMessage(conn)
		if err != nil {
			return
		}
		switch in.T {
		case "input":
			if err := sess.pty.Write([]byte(in.Data)); err != nil {
				return
			}
		case "resize":
			_ = sess.pty.Resize(in.size())
		case "takeover":
			// Asked for at any time, not only when the cap is hit: it is how a
			// window takes back the slots held by sessions the user cannot see.
			if n := s.terms.killOthers(user, sess.token); n > 0 {
				_ = s.db.AddAuditLog("000+"+user, "terminal.takeover")
			}
			s.notifyOthers(user)
		case "bye":
			// The window is going away for good: end the shell now rather than
			// holding it for the grace period.
			sess.close()
			return
		}
	}
}

// notifyOthers pushes the current count of *other* shells to each of the user's
// attached windows, so the notice line stays true as windows come and go.
func (s *Server) notifyOthers(user string) {
	for _, sess := range s.terms.attached(user) {
		sess.mu.Lock()
		sink := sess.sink
		sess.mu.Unlock()
		if sink == nil {
			continue
		}
		_ = sink.write(websocket.TextMessage, mustJSON(termOut{
			T: "others", Others: s.terms.others(user, sess.token),
		}))
	}
}

// readTermMessage reads one control frame.
func readTermMessage(conn *websocket.Conn) (termIn, error) {
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return termIn{}, err
	}
	var in termIn
	if err := json.Unmarshal(msg, &in); err != nil {
		return termIn{}, err
	}
	return in, nil
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
	if !s.cfg.Panel.WebSSH {
		featureless404(w)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(termJS)
}
