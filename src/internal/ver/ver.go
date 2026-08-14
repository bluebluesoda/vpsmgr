package ver

// Version is the vpsmgr release version. CI overrides it at build time via
// -ldflags "-X vpsmgr/internal/ver.Version=...". Dev builds default to 0.1.0.
var Version = "0.1.0"
