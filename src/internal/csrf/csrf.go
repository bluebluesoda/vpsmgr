// Package csrf provides origin-based CSRF defense for the panels' POST
// routes. Both panels already enforce SameSite=Lax on their session cookies
// (which stops cross-site POSTs from carrying a session) and POST-only for
// state changes; Allowed closes the remaining vectors — same-site subdomain
// POSTs and login CSRF (which needs no session cookie) — by rejecting any
// POST whose Origin / Sec-Fetch-Site says it is not same-origin.
package csrf

import (
	"net/http"
	"strings"
)

// Allowed reports whether a POST request is same-origin. Browsers always send
// an Origin header (and Sec-Fetch-Site) on cross-origin POSTs — form posts and
// fetch alike — so a mismatch with the request's own Host means a cross-site
// or same-site-subdomain request. Requests carrying neither header are
// allowed: older clients omit them and SameSite=Lax still protects those.
func Allowed(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return true
	}
	// Sec-Fetch-Site is authoritative when present and needs no host parsing:
	// 'same-origin' and 'none' (a direct, user-initiated request) are the only
	// acceptable sources for a state-changing POST; 'cross-site' and 'same-site'
	// (a sibling subdomain) are rejected.
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" {
		return s == "same-origin" || s == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // no origin information; SameSite=Lax still applies
	}
	return originHost(origin) == r.Host
}

// originHost strips the scheme (and trailing path, if any) from an Origin
// header, leaving "host[:port]" to compare against r.Host.
func originHost(origin string) string {
	if i := strings.Index(origin, "://"); i >= 0 {
		origin = origin[i+3:]
	}
	if i := strings.IndexByte(origin, '/'); i >= 0 {
		origin = origin[:i]
	}
	return origin
}
