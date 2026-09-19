package mgr

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"vpsmgr/internal/cert"
)

// temporary transfer listener
//
// `vps transfer send` exports the container to a tarball and then serves that
// one file over HTTPS until the peer has it. The listener is deliberately
// separate from the panel: it lives only for the duration of the command, it
// presents a throwaway self-signed certificate whose fingerprint the peer pins,
// and it exposes exactly one path. Reusing the panel's port would mean carving
// an unauthenticated hole in the session/CSRF middleware that guards the
// operator surface, and reusing its certificate would make a long-lived
// identity the secret that protects a one-off transfer.

// stallTimeout is how long an open connection may move no bytes at all before
// it is treated as dead and dropped. Without it a half-open connection left by
// a peer that vanished would keep the listener alive forever, since the idle
// countdown only starts once nothing is connected.
const stallTimeout = 5 * time.Minute

// TransferStats is one second's worth of listener state, handed to the CLI so
// it can print progress (and keep the SSH session from timing out on silence).
type TransferStats struct {
	// Active is the number of open connections.
	Active int
	// Connected is how long the current transfer has been running (0 when
	// nothing is connected).
	Connected time.Duration
	// IdleLeft is how much of the idle budget is left before the listener
	// gives up (0 while a transfer is in progress).
	IdleLeft time.Duration
	// Served is the total number of bytes written to peers.
	Served int64
	// BytesPerSecond is the throughput of the current connection.
	BytesPerSecond int64
}

// TransferFiles are the two artefacts a send publishes: the container's disk,
// and the small JSON describing the account it belongs to.
type TransferFiles struct {
	Archive    string
	ArchiveSHA string
	Meta       string
	MetaSHA    string
}

// TransferServer serves one exported container to whoever holds the token.
type TransferServer struct {
	files  TransferFiles
	token  string
	url    string
	finger string

	ln  net.Listener
	srv *http.Server

	mu             sync.Mutex
	conns          map[net.Conn]struct{}
	connectedAt    time.Time
	lastProgress   time.Time
	lastSample     time.Time
	served         int64
	servedAtSample int64
	speed          int64
}

// NewTransferServer starts a listener on port (0 picks a free one) and returns
// the server, the URL to hand to the receiving host and the SHA-256 fingerprint
// of its certificate. host is the address the peer should dial (the panel's
// public address).
//
// The two files are served from the one token, at /d/ (the disk) and /m/ (the
// account metadata, which the receiver fetches first so it can check it has
// room before pulling gigabytes). The caller owns both files and is responsible
// for deleting them.
func NewTransferServer(files TransferFiles, host string, port int) (*TransferServer, error) {
	// The host is whatever the operator advertises as this machine's address
	// (panel.display_ip, falling back to public_ip): usually an IPv4 literal,
	// but a DNS name dials exactly the same. The peer pins the certificate by
	// its fingerprint rather than checking a hostname, so the only requirement
	// is that the host survives being embedded in a URL.
	if host == "" || strings.ContainsAny(host, " \t/?#@:") {
		return nil, fmt.Errorf("cannot build a transfer address from %q", host)
	}
	certPEM, keyPEM, der, err := cert.SelfSigned(host)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return nil, err
	}
	token, err := newShareCode()
	if err != nil {
		ln.Close()
		return nil, err
	}
	actual := ln.Addr().(*net.TCPAddr).Port
	sum := sha256.Sum256(der)
	finger := hex.EncodeToString(sum[:])
	s := &TransferServer{
		files: files,
		token: token,
		// All three digests ride in the fragment: HTTP clients never send a
		// fragment, so the listener is not told the checksums of what it is
		// serving, nor the fingerprint of its own certificate.
		url: fmt.Sprintf("https://%s:%d/d/%s#sha256=%s&meta=%s&cert=%s",
			host, actual, token, files.ArchiveSHA, files.MetaSHA, finger),
		finger:       finger,
		ln:           ln,
		conns:        map[net.Conn]struct{}{},
		lastProgress: time.Now(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/d/", s.handle(s.files.Archive, "/d/"))
	mux.HandleFunc("/m/", s.handle(s.files.Meta, "/m/"))
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		ConnState:         s.connState,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12},
	}
	return s, nil
}

// URL is the command-line argument the receiving host runs.
func (s *TransferServer) URL() string { return s.url }

// Fingerprint is the SHA-256 of the listener's certificate, in hex.
func (s *TransferServer) Fingerprint() string { return s.finger }

// Port is the port the listener actually bound.
func (s *TransferServer) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// Serve blocks until the idle budget runs out or the listener fails, reporting
// state to tick once a second. A transfer may be interrupted and resumed as
// often as the peer likes: the file is complete on disk before serving starts,
// so every connection is an ordinary Range-capable download.
func (s *TransferServer) Serve(idle time.Duration, tick func(TransferStats)) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.srv.ServeTLS(s.ln, "", "") }()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var idleFor time.Duration
	for {
		select {
		case err := <-serveErr:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ticker.C:
			now := time.Now()
			if s.stalled(now) {
				s.dropConns()
			}
			if s.connCount() == 0 {
				idleFor += time.Second
				if idleFor >= idle {
					s.Close()
					continue // ServeTLS then returns ErrServerClosed
				}
			} else {
				idleFor = 0
			}
			if tick != nil {
				st := s.sample(now)
				st.IdleLeft = idle - idleFor
				tick(st)
			}
		}
	}
}

// Close shuts the listener down and drops any connection still open. It is safe
// to call more than once and does not touch the exported file.
func (s *TransferServer) Close() error {
	s.dropConns()
	return s.srv.Close()
}

// handle serves one of the published files, only at the path its prefix names
// and only to a request carrying the token.
func (s *TransferServer) handle(path, prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.URL.Path), []byte(prefix+s.token)) != 1 {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			http.Error(w, "transfer file is gone", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			http.Error(w, "transfer file is unreadable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		// ServeContent handles HEAD, Range and If-Range from the file itself,
		// which is what lets an interrupted download resume where it stopped.
		http.ServeContent(w, r, "", st.ModTime(), &countingFile{f: f, s: s})
	}
}

func (s *TransferServer) connState(c net.Conn, st http.ConnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch st {
	case http.StateNew:
		s.conns[c] = struct{}{}
		if s.connectedAt.IsZero() {
			s.connectedAt = time.Now()
		}
		s.lastProgress = time.Now()
	case http.StateClosed, http.StateHijacked:
		delete(s.conns, c)
		if len(s.conns) == 0 {
			s.connectedAt = time.Time{}
			s.speed = 0
		}
	}
}

func (s *TransferServer) connCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// stalled reports whether a connection has been open for a full stallTimeout
// without moving a single byte — a peer that disappeared without closing.
func (s *TransferServer) stalled(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns) > 0 && now.Sub(s.lastProgress) > stallTimeout
}

func (s *TransferServer) dropConns() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.conns = map[net.Conn]struct{}{}
	s.connectedAt = time.Time{}
	s.lastProgress = time.Now()
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (s *TransferServer) addServed(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.served += n
	s.lastProgress = time.Now()
}

func (s *TransferServer) sample(now time.Time) TransferStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lastSample.IsZero() {
		if dt := now.Sub(s.lastSample).Seconds(); dt > 0 {
			s.speed = int64(float64(s.served-s.servedAtSample) / dt)
		}
	}
	s.lastSample = now
	s.servedAtSample = s.served
	st := TransferStats{Active: len(s.conns), Served: s.served, BytesPerSecond: s.speed}
	if st.Active > 0 && !s.connectedAt.IsZero() {
		st.Connected = now.Sub(s.connectedAt)
	}
	return st
}

// countingFile feeds http.ServeContent while recording how much actually
// reached the peer, so the CLI can show progress and detect a stalled transfer.
type countingFile struct {
	f *os.File
	s *TransferServer
}

func (c *countingFile) Read(p []byte) (int, error) {
	n, err := c.f.Read(p)
	if n > 0 {
		c.s.addServed(int64(n))
	}
	return n, err
}

func (c *countingFile) Seek(offset int64, whence int) (int64, error) {
	return c.f.Seek(offset, whence)
}

var _ io.ReadSeeker = (*countingFile)(nil)
