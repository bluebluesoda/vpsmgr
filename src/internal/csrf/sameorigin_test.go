package csrf

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		site   string
		want   bool
	}{
		{"matching origin", "http://example.com", "", true},
		{"matching origin with scheme mismatch", "https://example.com", "", true},
		{"foreign origin", "http://evil.test", "", false},
		{"subdomain", "http://a.example.com", "", false},
		{"no headers at all", "", "", false},
		{"site same-origin only", "", "same-origin", true},
		{"site none only", "", "none", true},
		{"site cross-site", "http://example.com", "cross-site", false},
		{"site same-site beats origin", "http://example.com", "same-site", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "http://example.com/terminal", nil)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if tc.site != "" {
			r.Header.Set("Sec-Fetch-Site", tc.site)
		}
		if got := SameOrigin(r); got != tc.want {
			t.Errorf("%s: SameOrigin = %v, want %v", tc.name, got, tc.want)
		}
	}
}
