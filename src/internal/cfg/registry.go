package cfg

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// FieldKind classifies how a config field may be touched. This is the single
// source of truth for "who can change what" — `vps config help` renders it and
// `vps config set` enforces it, so there is no ambiguity between editable and
// fixed fields.
type FieldKind int

const (
	// KindImmutable is fixed at install; changing it would break existing
	// containers/wiring. Refused by `vps config set` (the two secret panel
	// paths are an exception: they may be SET when currently empty, i.e.
	// re-enabling a disabled panel).
	KindImmutable FieldKind = iota
	// KindOperator is operator-editable; the Apply column says how the change
	// takes effect (panel restart, `vps install`, next add, ...).
	KindOperator
	// KindRuntime is a live toggle applied immediately (net.v4_forward).
	KindRuntime
	// KindAuto is written by the panel/installer only; never user-set.
	KindAuto
	// KindSpecial is managed through a dedicated command (admin_pass_hash via
	// `vps admin-passwd`).
	KindSpecial
)

func (k FieldKind) String() string {
	switch k {
	case KindImmutable:
		return "fixed at install"
	case KindOperator:
		return "operator"
	case KindRuntime:
		return "runtime toggle"
	case KindAuto:
		return "auto (panel-managed)"
	case KindSpecial:
		return "managed elsewhere"
	}
	return "?"
}

// Apply describes how a change to a field takes effect.
type Apply int

const (
	ApplyRestart Apply = iota // panel is restarted automatically by `vps config set`
	ApplyInstall              // re-run `vps install` (or `vps config set --apply`)
	ApplyImmediate            // applied by vps config set right now
	ApplyNextAdd              // used on the next vps add / reinstall
	ApplyNone                 // not settable
)

func (a Apply) String() string {
	switch a {
	case ApplyRestart:
		return "restart panel (auto)"
	case ApplyInstall:
		return "re-run vps install"
	case ApplyImmediate:
		return "applied immediately"
	case ApplyNextAdd:
		return "next add / reinstall"
	case ApplyNone:
		return "-"
	}
	return "?"
}

// Field describes one config key for the `vps config` command.
type Field struct {
	Key     string
	Kind    FieldKind
	Apply   Apply
	Desc    string
	Example string // shown by the interactive `vps config set` prompt
	Get     func(c *Config) string
	Assign  func(c *Config, v string) error
}

// AssignCheck validates an input value for a field by applying it to a COPY of
// the config — the caller's config is not mutated. Used by the interactive
// `vps config set` prompt to check-and-reprompt on bad input.
func AssignCheck(c *Config, f *Field, v string) error {
	cp := *c
	return f.Assign(&cp, v)
}

// Editable reports whether the field may be changed via `vps config set`:
// "yes", "no", or "only-when-empty" (the two secret panel paths can be set to
// re-enable a disabled panel, but are frozen once set).
func (f *Field) Editable() string {
	switch f.Kind {
	case KindOperator, KindRuntime:
		return "yes"
	case KindImmutable:
		if f.Key == "panel.url_path" || f.Key == "panel.admin_url_path" {
			return "only-when-empty"
		}
	}
	return "no"
}

// Fields is the registry backing `vps config list/set/help`.
var Fields = []Field{
	{"panel.listen", KindOperator, ApplyRestart,
		"panel listen address, e.g. \":8443\"",
		":8443",
		getStr(func(c *Config) string { return c.Panel.Listen }),
		func(c *Config, v string) error {
			if _, _, err := net.SplitHostPort(v); err != nil {
				return fmt.Errorf("panel.listen must be host:port (e.g. \":8443\")")
			}
			c.Panel.Listen = v
			return nil
		}},
	{"panel.cert", KindOperator, ApplyRestart, "panel TLS certificate path",
		"/etc/vpsmgr/panel.crt",
		getStr(func(c *Config) string { return c.Panel.Cert }),
		nonEmpty("panel.cert", func(c *Config, v string) { c.Panel.Cert = v })},
	{"panel.key", KindOperator, ApplyRestart, "panel TLS private key path",
		"/etc/vpsmgr/panel.key",
		getStr(func(c *Config) string { return c.Panel.Key }),
		nonEmpty("panel.key", func(c *Config, v string) { c.Panel.Key = v })},
	{"panel.db", KindOperator, ApplyInstall, "SQLite database path",
		"/etc/vpsmgr/vpsmgr.db",
		getStr(func(c *Config) string { return c.Panel.DB }),
		nonEmpty("panel.db", func(c *Config, v string) { c.Panel.DB = v })},
	{"panel.public_ip", KindOperator, ApplyInstall, "NIC IPv4 used by firewall/routing (cert is regenerated)",
		"203.0.113.5 or AUTO",
		getStr(func(c *Config) string { return c.Panel.PublicIP }),
		func(c *Config, v string) error {
			v = strings.TrimSpace(v)
			if v == "" || v == "AUTO" {
				c.Panel.PublicIP = ""
				return nil
			}
			if net.ParseIP(v) == nil {
				return fmt.Errorf("panel.public_ip must be a valid IP address or AUTO")
			}
			c.Panel.PublicIP = v
			return nil
		}},
	{"panel.display_ip", KindOperator, ApplyRestart, "public IPv4 shown to users (panel URL / SSH hints); empty = fall back to public_ip",
		"203.0.113.5 or AUTO",
		getStr(func(c *Config) string { return c.Panel.DisplayIP }),
		func(c *Config, v string) error {
			v = strings.TrimSpace(v)
			if v == "" || v == "AUTO" {
				c.Panel.DisplayIP = ""
				return nil
			}
			if net.ParseIP(v) == nil {
				return fmt.Errorf("panel.display_ip must be a valid IP address, or empty/AUTO")
			}
			c.Panel.DisplayIP = v
			return nil
		}},
	{"panel.session_days", KindOperator, ApplyRestart, "login session lifetime in days",
		"3",
		getStr(func(c *Config) string { return strconv.Itoa(c.Panel.SessionDays) }),
		func(c *Config, v string) error {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n <= 0 {
				return fmt.Errorf("panel.session_days must be a positive integer (days)")
			}
			c.Panel.SessionDays = n
			return nil
		}},
	{"panel.url_path", KindImmutable, ApplyRestart,
		"secret prefix of the user panel (immutable once set; settable only while empty = enable)",
		"Ab1_cdEf-9x",
		getStr(func(c *Config) string { return c.Panel.URLPath }),
		func(c *Config, v string) error {
			if err := setSecretPath(v, c.Panel.AdminPath, "panel.url_path"); err != nil {
				return err
			}
			c.Panel.URLPath = v
			return nil
		}},
	{"panel.admin_url_path", KindOperator, ApplyRestart,
		"secret prefix of the admin panel; empty value = admin panel disabled",
		"Xy-9ab_cdEf (or empty/CLEAR to disable)",
		func(c *Config) string {
			if c.Panel.AdminPath == "" {
				return "disabled"
			}
			return c.Panel.AdminPath
		},
		func(c *Config, v string) error {
			v = strings.TrimSpace(v)
			if v == "" {
				c.Panel.AdminPath = ""
				return nil
			}
			if err := setSecretPath(v, c.Panel.URLPath, "panel.admin_url_path"); err != nil {
				return err
			}
			c.Panel.AdminPath = v
			return nil
		}},
	{"panel.admin_pass_hash", KindSpecial, ApplyNone,
		"admin password bcrypt hash — set via `vps admin-passwd` (stored in the DB)",
		"",
		func(c *Config) string { return "(in database, via `vps admin-passwd`)" },
		func(c *Config, v string) error {
			return fmt.Errorf("panel.admin_pass_hash is managed by `vps admin-passwd`, not `vps config set`")
		}},
	{"net.subnet", KindImmutable, ApplyNone,
		"container subnet 10.<n>.0.0/24 — fixed at install, would break existing containers",
		"",
		getStr(func(c *Config) string { return c.Net.Subnet }),
		func(c *Config, v string) error {
			return fmt.Errorf("net.subnet is fixed at install; changing it breaks existing containers")
		}},
	{"net.gateway", KindImmutable, ApplyNone,
		"bridge gateway (derived from subnet) — fixed at install",
		"",
		getStr(func(c *Config) string { return c.Net.Gateway }),
		func(c *Config, v string) error {
			return fmt.Errorf("net.gateway is fixed at install (derived from net.subnet)")
		}},
	{"net.v4_forward", KindRuntime, ApplyImmediate,
		"IPv4 inbound policy: true = SSH/port-block DNAT + traefik, false = IPv6-only containers",
		"true or false",
		getStr(func(c *Config) string { return strconv.FormatBool(c.Net.V4Forward) }),
		func(c *Config, v string) error {
			b, ok := parseBool(v)
			if !ok {
				return fmt.Errorf("net.v4_forward must be true/false or 1/0")
			}
			c.Net.V4Forward = b
			return nil
		}},
	{"net.ext_if", KindOperator, ApplyInstall, "external NIC (auto-detected from default route)",
		"eth0",
		getStr(func(c *Config) string { return c.Net.ExtIF }),
		nonEmpty("net.ext_if", func(c *Config, v string) { c.Net.ExtIF = v })},
	{"net.ipv6_subnet", KindOperator, ApplyInstall,
		"global IPv6 prefix for pass-through (e.g. 2602:fada:6::/64); empty = disabled",
		"2602:fada:6::/64 (empty to disable)",
		func(c *Config) string {
			if c.Net.IPv6Subnet == "" {
				return "disabled"
			}
			return c.Net.IPv6Subnet
		},
		func(c *Config, v string) error {
			v = strings.TrimSpace(v)
			if v == "" {
				c.Net.IPv6Subnet = ""
				return nil
			}
			old := c.Net.IPv6Subnet
			c.Net.IPv6Subnet = v
			if _, err := c.IPv6Network(); err != nil {
				c.Net.IPv6Subnet = old
				return err
			}
			return nil
		}},
	{"incus.image", KindOperator, ApplyNextAdd, "container image alias used on add/reinstall",
		"vpsmgr/debian-sshd",
		getStr(func(c *Config) string { return c.Incus.Image }),
		nonEmpty("incus.image", func(c *Config, v string) { c.Incus.Image = v })},
	{"incus.image_fallback", KindOperator, ApplyNextAdd, "fallback remote image when the local one is missing",
		"images:debian/13",
		getStr(func(c *Config) string { return c.Incus.ImageFallback }),
		nonEmpty("incus.image_fallback", func(c *Config, v string) { c.Incus.ImageFallback = v })},
	{"incus.pool", KindImmutable, ApplyNone,
		"storage pool — fixed at install",
		"",
		getStr(func(c *Config) string { return c.Incus.Pool }),
		func(c *Config, v string) error {
			return fmt.Errorf("incus.pool is fixed at install; changing it breaks existing containers")
		}},
	{"incus.bridge", KindImmutable, ApplyNone,
		"managed bridge — fixed at install",
		"",
		getStr(func(c *Config) string { return c.Incus.Bridge }),
		func(c *Config, v string) error {
			return fmt.Errorf("incus.bridge is fixed at install; changing it breaks existing containers")
		}},
	{"incus.socket", KindOperator, ApplyRestart, "Incus daemon Unix socket path",
		"/var/lib/incus/unix.socket",
		getStr(func(c *Config) string { return c.Incus.Socket }),
		nonEmpty("incus.socket", func(c *Config, v string) { c.Incus.Socket = v })},
	{"installed_version", KindAuto, ApplyNone, "binary version that installed/adopted this config (auto)",
		"",
		getStr(func(c *Config) string { return c.InstalledVersion }),
		func(c *Config, v string) error {
			return fmt.Errorf("installed_version is auto-written by `vps install`")
		}},
	{"uninstalled_version", KindAuto, ApplyNone, "binary version that a non-purging uninstall removed (auto)",
		"",
		getStr(func(c *Config) string { return c.UninstalledVersion }),
		func(c *Config, v string) error {
			return fmt.Errorf("uninstalled_version is auto-written by `vps note-version`")
		}},
}

var secretPathRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// setSecretPath validates a secret panel path for the enable case: at least 10
// chars, safe charset, and different from the other panel's path.
func setSecretPath(v, other, key string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("%s cannot be empty", key)
	}
	if len(v) < 10 {
		return fmt.Errorf("%s must be at least 10 characters (got %d)", key, len(v))
	}
	if !secretPathRe.MatchString(v) {
		return fmt.Errorf("%s must contain only [A-Za-z0-9_-]", key)
	}
	if other != "" && v == other {
		return fmt.Errorf("%s must differ from the other panel path", key)
	}
	return nil
}

func getStr(f func(c *Config) string) func(c *Config) string { return f }

// nonEmpty returns an Assign that trims and requires a non-empty value.
func nonEmpty(key string, set func(c *Config, v string)) func(c *Config, v string) error {
	return func(c *Config, v string) error {
		v = strings.TrimSpace(v)
		if v == "" {
			return fmt.Errorf("%s must not be empty", key)
		}
		set(c, v)
		return nil
	}
}

func parseBool(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

// FieldFor returns the registry entry for a key, or nil.
func FieldFor(key string) *Field {
	for i := range Fields {
		if Fields[i].Key == key {
			return &Fields[i]
		}
	}
	return nil
}

// FieldValue returns the current effective value of a registry key.
func FieldValue(c *Config, key string) string {
	if f := FieldFor(key); f != nil && f.Get != nil {
		return f.Get(c)
	}
	return ""
}
