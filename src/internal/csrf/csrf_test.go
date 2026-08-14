package csrf

import (
	"net/http/httptest"
	"testing"
)

func TestAllowed(t *testing.T) {
	const host = "panel.example.com"
	const sameOrigin = "https://" + host

	cases := []struct {
		name    string
		method  string
		origin  string
		secSite string
		want    bool
	}{
		{"GET is always allowed", "GET", "", "", true},
		{"POST no headers allowed (SameSite=Lax covers)", "POST", "", "", true},
		{"same-origin sec-fetch-site", "POST", "", "same-origin", true},
		{"none sec-fetch-site (user-initiated)", "POST", "", "none", true},
		{"cross-site sec-fetch-site", "POST", "", "cross-site", false},
		{"same-site subdomain sec-fetch-site", "POST", "", "same-site", false},
		{"matching Origin", "POST", sameOrigin, "", true},
		{"mismatched Origin (cross-origin POST)", "POST", "https://evil.example.com", "", false},
		{"Origin on different port", "POST", "https://10.1.2.3:9999", "", false},
		{"Origin wins over absent sec-fetch-site", "POST", sameOrigin, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, "https://"+host+"/x", nil)
			r.Host = host
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if c.secSite != "" {
				r.Header.Set("Sec-Fetch-Site", c.secSite)
			}
			if got := Allowed(r); got != c.want {
				t.Errorf("Allowed = %v, want %v", got, c.want)
			}
		})
	}
}

func TestOriginHost(t *testing.T) {
	cases := map[string]string{
		"https://panel.example.com":      "panel.example.com",
		"http://10.0.0.1:8443":           "10.0.0.1:8443",
		"https://example.com/path?q=1":   "example.com",
		"http://unix.example.com:8080/x": "unix.example.com:8080",
	}
	for in, want := range cases {
		if got := originHost(in); got != want {
			t.Errorf("originHost(%q) = %q, want %q", in, got, want)
		}
	}
}
