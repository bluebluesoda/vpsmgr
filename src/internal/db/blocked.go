package db

// DefaultBlockedDomains is the initial admin blocked-domains list, seeded once
// by migration v5 (settings key absent only). The admin edits/removes entries
// in the web UI; that state is never re-seeded.
var DefaultBlockedDomains = []string{
	"mozilla.org",
	"mozilla.net",
	"github.com",
	"githubusercontent.com",
	"github.io",
	"awsstatic.com",
	"microsoft.com",
	"apple.com",
	"cdn-apple.com",
	"icloud.com",
	"google.com",
}
