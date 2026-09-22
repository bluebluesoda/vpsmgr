package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/lx"
	"vpsmgr/internal/markdown"
	"vpsmgr/internal/mgr"
	"vpsmgr/internal/pw"
)

type ctxKey int

const adminKey ctxKey = 0

// loginDummyHash is compared against when no admin password is set yet so the
// login takes the same time whether or not an admin exists.
var loginDummyHash = func() string { h, _ := pw.Hash("vpsmgr-admin-timing-pad"); return h }()

// pageData is the data handed to the admin templates.
type pageData struct {
	Title       string
	Prefix      string
	Msg         string
	Err         string
	Host        hostView
	Reboot      bool
	Users       []userView
	UserCount   int
	MaxUsers    int
	CapacityPct int
	V4Forward   bool
	Lang        string
	// Colors is the fixed palette of accent colors an admin may assign to a
	// user (see userColorPalette). Empty when never rendered.
	Colors []string
	// IPv6 pool mode: the free addresses offered in the create-user dropdown
	// (empty = not pool mode / pool exhausted). PoolUsed/PoolTotal are the
	// pool fill for display.
	IPv6PoolMode bool
	PoolFree     []string
	PoolUsed     int
	PoolTotal    int
	// Whole /64 blocks (net.ipv6_extra_prefix, prefix mode only). ExtraConfigured
	// is "the operator set an extra prefix", which is what makes the create
	// form's checkbox render at all; ExtraFree is how many blocks are left, and
	// 0 greys the checkbox out instead of hiding it.
	ExtraConfigured bool
	ExtraFree       int
	// AdminKeys is the operator's own SSH-key store (management panel only).
	AdminKeys []sshKeyRow
	// BatchID, when set, is a running/recent batch create the page should show
	// the progress modal for (carried through the redirect after submitting).
	BatchID string
	// Global dynamic CPU limit rule, edited by the card below (the DB is the
	// single source of truth — there is no config.yaml / CLI equivalent). The
	// Active list is the containers currently capped by it.
	CPULimitEnabled bool
	CPULimitMinutes int
	CPULimitPercent int
	CPULimitCores   string
	CPULimitHours   int
	CPULimitDurMin  int
	CPULimitActive  []cpuLimitRow
}

// cpuLimitRow is one container currently under the dynamic CPU limit, shown in
// the rule card (name, the cap, and when it expires).
type cpuLimitRow struct {
	Name    string
	Cores   string
	Expires string
}

// sshKeyRow is one public key shown in the admin SSH-key management panel.
type sshKeyRow struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Key    string `json:"key"` // clean "type base64" (comment stripped)
	Active bool   `json:"active"`
}

// hostView carries host memory/swap/pool/uptime numbers for the overview cards.
type hostView struct {
	MemTotal  string
	MemUsed   string
	MemPct    string
	SwapTotal string
	SwapUsed  string
	SwapPct   string
	PoolTotal string
	PoolUsed  string
	PoolAvail string
	PoolPct   string
	Uptime    string
}

// userView is one row of the admin user table.
type userView struct {
	Name string
	// Color is the operator-assigned accent color ("" = default). It colors
	// the bold username and the "login panel" button for quick user spotting.
	Color string
	State string
	// Status is the persistent lifecycle state (ready/creating/reinstalling/
	// failed). Non-ready states are shown to the operator so a crashed
	// Add/Reinstall is visible instead of looking like a healthy user.
	Status      string
	Ports       string // full user-port block, e.g. 10700-10799 (tooltip)
	PortsShort  string // compact form, e.g. 107xx
	SSHPort     string
	QuotaCPU    string
	QuotaMem    string
	QuotaDisk   string
	BandwidthGB int // monthly bandwidth quota GiB, 0 = unlimited
	CPUUse      string
	MemUse      string
	DiskUsed    string
	UpGB        string
	DownGB      string
	BWTotal     string // up+down this month, GB — used for table sorting
	IPv6        string
	// IPv6Extra is the whole /64 the account owns from net.ipv6_extra_prefix
	// ("" when none). Shown read-only in the quota dialog, where such a block
	// can be assigned but never taken back.
	IPv6Extra  string
	Procs      int64  // live process count (0 when stopped)
	ProcsLimit string // per-container pids.max cap, e.g. "4096"
	// Quota validity: Expired locks the account to read-only (only the admin's
	// extend/delete work). ExpiredDays is whole days past the deadline, floored
	// (a same-day expiry shows 0). ExpiresShort is the UTC date for display.
	ExpiresAt    string
	ExpiresShort string
	Expired      bool
	ExpiredDays  int
}

func (s *Server) buildPageData(msg, errMsg string) pageData {
	d := pageData{
		Title:     "VPS Manager Admin",
		Prefix:    s.prefix(),
		Msg:       msg,
		Err:       errMsg,
		V4Forward: s.mgr.V4ForwardLive(),
		Colors:    append([]string{}, userColorPalette...),
	}
	hs := s.mgr.HostStats()
	d.Reboot = hs.RebootNeeded
	d.Host = hostView{
		MemTotal:  humanBytes(int64(hs.Mem.MemTotal)),
		MemUsed:   humanBytes(int64(hs.Mem.MemUsed)),
		SwapTotal: humanBytes(int64(hs.Mem.SwapTotal)),
		SwapUsed:  humanBytes(int64(hs.Mem.SwapUsed)),
		PoolTotal: humanBytes(hs.PoolTotal),
		PoolUsed:  humanBytes(hs.PoolUsed),
		PoolAvail: humanBytes(hs.PoolAvail),
		Uptime:    formatUptime(hs.Uptime),
	}
	if hs.Mem.MemTotal > 0 {
		d.Host.MemPct = strconv.Itoa(int(hs.Mem.MemUsed*100/hs.Mem.MemTotal)) + "%"
	}
	if hs.Mem.SwapTotal > 0 {
		d.Host.SwapPct = strconv.Itoa(int(hs.Mem.SwapUsed*100/hs.Mem.SwapTotal)) + "%"
	}
	if hs.PoolTotal > 0 {
		d.Host.PoolPct = strconv.Itoa(int(hs.PoolUsed*100/hs.PoolTotal)) + "%"
	}
	// IPv6 pool mode: offer the free addresses in the create dropdown.
	if s.mgr.IPv6Mode() == cfg.IPv6ModePool {
		d.IPv6PoolMode = true
		d.PoolFree = s.mgr.FreePoolIPv6List()
		if total, used, err := s.mgr.IPv6PoolUsage(); err == nil {
			d.PoolUsed, d.PoolTotal = used, total
		}
	}
	// Whole /64 blocks: prefix or none mode with an extra prefix configured. A prefix
	// that holds no whole /64 (a /64 or longer) counts as not configured — there
	// would be nothing to hand out.
	if s.mgr.IPv6Mode() == cfg.IPv6ModePrefix || s.mgr.IPv6Mode() == cfg.IPv6ModeNone {
		if total, _, _, free, err := s.mgr.ExtraCapacity(); err == nil && total > 0 {
			d.ExtraConfigured = true
			d.ExtraFree = free
		}
	}
	if keys, err := s.mgr.ListAdminKeys(); err == nil {
		for _, k := range keys {
			d.AdminKeys = append(d.AdminKeys, sshKeyRow{ID: k.ID, Name: k.Name, Key: k.Key, Active: k.Active})
		}
	}
	rule := s.mgr.CPULimitRule()
	d.CPULimitEnabled = rule.Enabled
	d.CPULimitMinutes = rule.WindowMinutes
	d.CPULimitPercent = rule.Percent
	d.CPULimitCores = mgr.FormatCPU(rule.CoresX10)
	d.CPULimitHours = rule.DurationSeconds / 3600
	d.CPULimitDurMin = (rule.DurationSeconds % 3600) / 60
	nowUnix := time.Now().Unix()
	for name, st := range s.mgr.CPULimits() {
		if st.Until <= nowUnix {
			continue
		}
		d.CPULimitActive = append(d.CPULimitActive, cpuLimitRow{
			Name:    name,
			Cores:   mgr.FormatCPU(st.CoresX10),
			Expires: fmtCPURemaining(time.Unix(st.Until, 0)),
		})
	}
	sort.Slice(d.CPULimitActive, func(i, j int) bool { return d.CPULimitActive[i].Name < d.CPULimitActive[j].Name })
	return d
}

// fmtCPURemaining renders the time left on a dynamic CPU limit compactly
// ("2h30m", "5m12s", "40s").
func fmtCPURemaining(until time.Time) string {
	d := time.Until(until).Truncate(time.Second)
	if d < 0 {
		d = 0
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

func (s *Server) loadUsers(d *pageData) {
	statuses, err := s.mgr.BatchUsers()
	if err != nil {
		d.Err = d.Err + " " + err.Error()
		return
	}
	vs := make([]userView, 0, len(statuses))
	now := time.Now().UTC()
	for _, st := range statuses {
		u := st.User
		expired := mgr.IsExpired(u.ExpiresAt, now)
		expDays := 0
		if expired {
			if rem := mgr.ExpiryRemaining(u.ExpiresAt, now); rem < 0 {
				expDays = int((-rem).Hours() / 24)
			}
		}
		expShort := u.ExpiresAt
		if len(expShort) >= 10 {
			expShort = expShort[:10]
		}
		vs = append(vs, userView{
			Name:         u.Name,
			Color:        u.Color,
			State:        st.State,
			Status:       u.Status,
			Ports:        mgr.UserPorts(u.StartPort, cfg.PortsPerUser),
			PortsShort:   mgr.UserPortsShort(u.StartPort),
			SSHPort:      strconv.Itoa(u.SSHPort),
			QuotaCPU:     mgr.FormatCPU(u.CPU),
			QuotaMem:     strconv.Itoa(u.MemMB) + " MiB",
			QuotaDisk:    strconv.Itoa(u.DiskGB) + " GiB",
			BandwidthGB:  u.BandwidthQuotaGB,
			CPUUse:       st.CPUUse,
			MemUse:       st.MemUse,
			DiskUsed:     st.DiskUsed,
			UpGB:         st.UpGB,
			DownGB:       st.DownGB,
			BWTotal:      st.BWTotal,
			IPv6:         st.IPv6,
			IPv6Extra:    st.IPv6Extra,
			Procs:        st.Procs,
			ProcsLimit:   lx.DefaultProcessesLimit,
			ExpiresAt:    u.ExpiresAt,
			ExpiresShort: expShort,
			Expired:      expired,
			ExpiredDays:  expDays,
		})
	}
	d.Users = vs
	d.UserCount = len(vs)
	// Capacity follows the configured user-port block count, not a fixed 200: a
	// narrowed net.user_ports lowers the ceiling, and existing users that fall
	// outside it (from a wider earlier setting) can push the count past 100% —
	// shown honestly as over-capacity.
	d.MaxUsers = s.cfg.UserPortCount()
	if d.MaxUsers > 0 {
		d.CapacityPct = d.UserCount * 100 / d.MaxUsers
		// With only a handful of containers the true percentage (e.g. 1/200)
		// is an invisible sliver; floor it at 2% so the bar is visibly present
		// once there is at least one container. 0 users stays empty.
		if d.UserCount > 0 && d.CapacityPct < 2 {
			d.CapacityPct = 2
		}
	}
}

// formatUptime renders a duration as a static non-ticking string like
// "5d 3h 12m" so the admin panel shows the uptime captured at page load.
func formatUptime(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	d = d.Round(time.Minute)
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	hours := d / time.Hour
	d -= hours * time.Hour
	mins := d / time.Minute
	if days > 0 {
		return strconv.FormatInt(int64(days), 10) + "d " + strconv.FormatInt(int64(hours), 10) + "h " + strconv.FormatInt(int64(mins), 10) + "m"
	}
	return strconv.FormatInt(int64(hours), 10) + "h " + strconv.FormatInt(int64(mins), 10) + "m"
}

// humanBytes renders a byte count as a short human string (e.g. "184 MiB").
func humanBytes(b int64) string {
	if b < 0 {
		b = 0
	}
	const unit = 1024
	if b < unit {
		return strconv.FormatInt(b, 10) + " B"
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(b)/float64(div), 'f', 1, 64) + " " + "KMGTPE"[exp:exp+1] + "iB"
}

// storeFlash persists a one-shot banner for the request's admin session.
func (s *Server) storeFlash(r *http.Request, msg, kind string) {
	if c, err := r.Cookie("vpsmgr_admin_session"); err == nil {
		if msg == "" {
			s.flash.Clear(c.Value)
			return
		}
		s.flash.Set(c.Value, msg, kind)
	}
}

// currentAdminHash reads the admin password hash fresh from the DB on every
// login. The CLI (`vps admin-passwd`) and the web UI both write the hash to
// the settings table, so a CLI reset is effective immediately without
// restarting the panel service. Login is low-frequency, so the extra read is
// negligible.
func (s *Server) currentAdminHash() string {
	v, _, err := s.db.GetSetting(db.SettingAdminPassHash)
	if err != nil {
		return ""
	}
	return v
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		ip := clientIP(r)
		s.limiter.prune()
		if !s.limiter.allowed(ip) {
			s.renderStatus(w, r, http.StatusTooManyRequests, "admin_login.html",
				pageData{Title: "Admin Login", Prefix: s.prefix(), Err: s.t(r, "err_too_many")})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		pass := r.FormValue("password")
		// Password-only login: no username. Compare against the configured
		// bcrypt hash; when unset (fresh install before admin-passwd) burn the
		// same bcrypt time as a real compare.
		hash := s.currentAdminHash()
		if hash == "" {
			pw.Verify(loginDummyHash, pass)
			s.render(w, r, "admin_login.html", pageData{Title: "Admin Login", Prefix: s.prefix(), Err: s.t(r, "err_not_configured")})
			return
		}
		if !pw.Verify(hash, pass) {
			s.render(w, r, "admin_login.html", pageData{Title: "Admin Login", Prefix: s.prefix(), Err: s.t(r, "err_bad_login")})
			return
		}
		token, err := s.sessions.create(s.cfg.Panel.SessionDays)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.setSessionCookie(w, token)
		_ = s.db.AddAuditLog("000", "session.login")
		s.redirect(w, r, s.p(""), "")
		return
	}
	s.render(w, r, "admin_login.html", pageData{Title: "Admin Login", Prefix: s.prefix()})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("vpsmgr_admin_session"); err == nil {
		s.sessions.delete(c.Value)
	}
	s.clearSessionCookie(w)
	s.redirect(w, r, s.p("/login"), "")
}

// handleOverview renders the admin dashboard. It performs one full batch
// refresh (a handful of incus calls) on every manual page load; there is no
// automatic polling.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	d := s.buildPageData("", "")
	d.BatchID = strings.TrimSpace(r.URL.Query().Get("batch"))
	s.loadUsers(&d)
	s.render(w, r, "admin_overview.html", d)
}

func (s *Server) handleFlash(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("vpsmgr_admin_session")
	if err != nil {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}
	msg, kind, data, _ := s.flash.Pop(c.Value)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Msg  string `json:"msg"`
		Kind string `json:"kind"`
		Data string `json:"data"`
	}{msg, kind, data})
}

// handleUserAdd creates a user with the CLI's Add logic and shows the full
// login credentials (panel address, username, password) once in a modal.
// userColorPalette is the fixed set of accent colors an admin can assign to a
// user. Black/white/gray are excluded on purpose (too faint against the
// panel's neutral surfaces); these mid-tone hues stay readable as button
// backgrounds in both light and dark mode. The stored value is the hex string
// itself, and this list is the allowlist — anything else is rejected.
var userColorPalette = []string{
	"#e11d48", // red
	"#ea580c", // orange
	"#d97706", // amber
	"#16a34a", // green
	"#0d9488", // teal
	"#0891b2", // cyan
	"#3b82f6", // blue
	"#7c3aed", // violet
	"#c026d3", // fuchsia
	"#db2777", // pink
}

// validUserColor reports whether color is a member of the fixed palette
// ("" is reserved for "reset to default" and handled by the caller).
func validUserColor(color string) bool {
	for _, c := range userColorPalette {
		if c == color {
			return true
		}
	}
	return false
}

func (s *Server) handleUserAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := strings.ToLower(strings.TrimSpace(r.FormValue("name")))
	if err := s.mgr.ValidateAddName(name, true); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	cpu, err := mgr.ParseCPU(r.FormValue("cpu"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	memMB, err := parseMem(r.FormValue("mem"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	diskGB, err := strconv.Atoi(r.FormValue("disk"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+s.t(r, "err_invalid_disk"))
		return
	}
	bandwidthGB, err := mgr.ParseBandwidthGB(r.FormValue("bandwidth"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	// IPv6 dropdown (pool mode only): "auto" (or empty) = first free pool
	// address, "none" = V4-only container, otherwise the picked address.
	ipv6 := strings.TrimSpace(r.FormValue("ipv6"))
	if ipv6 == "auto" {
		ipv6 = ""
	}
	days, err := strconv.Atoi(strings.TrimSpace(r.FormValue("days")))
	if err != nil || days < 0 {
		s.redirect(w, r, s.p(""), "error: "+s.t(r, "err_invalid_days"))
		return
	}
	// Whole-/64 checkbox (prefix mode with an extra prefix configured): the form
	// ticks it by default, and it is disabled once the pool is empty. Allocation
	// is best-effort either way — an exhausted pool silently yields no block.
	allocExtra64 := strings.TrimSpace(r.FormValue("extra64")) != ""
	res, err := s.mgr.Add(name, mgr.AddOptions{CPU: cpu, MemMB: memMB, DiskGB: diskGB, BandwidthGB: bandwidthGB, IPv6Addr: ipv6, AllowChild: true, Days: days, AllocateExtra64: allocExtra64})
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000+"+name, "user.create")
	// The password is always auto-generated and shown once. The panel address
	// is taken from the request's own Host (see panelURL), so it matches
	// whatever origin the operator actually used to reach the admin panel.
	cred := "user:      " + res.User.Name
	if res.Password != "" {
		cred += "\npassword:  " + res.Password
	} else {
		cred += "\npassword:  same as the existing user-group password"
	}
	cred += "\npanel:     " + s.panelURL(r, "/"+s.cfg.Panel.URLPath)
	// Carry the username as flash data so the modal can offer "log in as".
	s.redirectModalData(w, r, s.p(""), cred, res.User.Name)
}

// handleUserBatch starts a background batch create that clones each user from a
// shared checkpoint. Everything is validated up front — share code, the whole
// name list, quotas, admin keys — so a rejected batch creates nothing at all.
func (s *Server) handleUserBatch(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if !s.mgr.SnapshotShareEnabled() {
		s.redirect(w, r, s.p(""), "error: snapshot sharing is disabled")
		return
	}
	share := strings.TrimSpace(r.FormValue("share"))
	if share == "" {
		s.redirect(w, r, s.p(""), "error: "+s.t(r, "batch_need_share"))
		return
	}
	// Validate the form first: a typo should be reported without an Incus
	// round-trip on the share code.
	names, err := s.mgr.ValidateBatchNames(strings.Split(r.FormValue("names"), "\n"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	cpu, err := mgr.ParseCPU(r.FormValue("cpu"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	memMB, err := parseMem(r.FormValue("mem"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	diskGB, err := strconv.Atoi(r.FormValue("disk"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+s.t(r, "err_invalid_disk"))
		return
	}
	bandwidthGB, err := mgr.ParseBandwidthGB(r.FormValue("bandwidth"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	days, err := strconv.Atoi(strings.TrimSpace(r.FormValue("days")))
	if err != nil || days < 0 {
		s.redirect(w, r, s.p(""), "error: "+s.t(r, "err_invalid_days"))
		return
	}
	keyIDs, err := s.parseAdminKeyIDs(r.Form["akeys"])
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	// Resolve last: this is the only check that has to ask the database and
	// Incus whether the checkpoint is still there. Doing it here means a dead
	// code fails once, up front, instead of N times inside the batch.
	if _, _, err := s.mgr.ResolveShare(share); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	id, err := s.batches.start(names)
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	opt := mgr.AddOptions{
		CPU: cpu, MemMB: memMB, DiskGB: diskGB, BandwidthGB: bandwidthGB,
		AllowChild: true, Days: days, FromShare: share,
		AllocateExtra64: strings.TrimSpace(r.FormValue("extra64")) != "",
	}
	_ = s.db.AddAuditLog("000", "user.batch_create")
	// Background: each clone takes tens of seconds, so the request returns at
	// once and the page polls for progress (and for the one-time passwords).
	go func() {
		s.mgr.AddBatch(names, opt, keyIDs, func(res mgr.BatchResult) {
			if res.State == mgr.BatchDone {
				_ = s.db.AddAuditLog("000+"+res.Name, "user.create")
			}
			s.batches.update(id, res)
		})
		s.batches.finish(id)
	}()
	s.redirect(w, r, s.p("/?batch="+id), s.t(r, "batch_started"))
}

// parseAdminKeyIDs validates the submitted admin-key selection against the
// operator's store, so a stale checkbox cannot grant a key that no longer
// exists.
func (s *Server) parseAdminKeyIDs(raw []string) ([]int64, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	keys, err := s.db.ListAdminKeys()
	if err != nil {
		return nil, err
	}
	known := make(map[int64]bool, len(keys))
	for _, k := range keys {
		known[k.ID] = true
	}
	out := make([]int64, 0, len(raw))
	for _, v := range raw {
		id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || !known[id] {
			return nil, errors.New("unknown admin key: " + v)
		}
		out = append(out, id)
	}
	return out, nil
}

// handleUserBatchStatus reports batch progress. The job id is random and only
// handed to the operator who started it; the one-time passwords ride along, so
// the response is never cached (Cache-Control: no-store is set panel-wide).
func (s *Server) handleUserBatchStatus(w http.ResponseWriter, r *http.Request) {
	job, ok := s.batches.get(strings.TrimSpace(r.URL.Query().Get("id")))
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(struct {
			OK    bool   `json:"ok"`
			Error string `json:"error,omitempty"`
		}{false, "unknown or expired batch"})
		return
	}
	json.NewEncoder(w).Encode(struct {
		OK bool `json:"ok"`
		batchJob
	}{true, job})
}

func (s *Server) handleUserDel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := r.FormValue("name")
	if r.FormValue("confirm") != "1" {
		s.redirect(w, r, s.p(""), "error: please confirm deletion")
		return
	}
	if err := s.mgr.Del(name); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000+"+name, "user.delete")
	s.redirect(w, r, s.p(""), s.t(r, "user_deleted", name))
}

func (s *Server) handleUserQuota(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := r.FormValue("name")
	cpu, err := mgr.ParseCPU(r.FormValue("cpu"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	memMB, err := parseMem(r.FormValue("mem"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	diskGB, err := strconv.Atoi(r.FormValue("disk"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+s.t(r, "err_invalid_disk"))
		return
	}
	bandwidthGB, err := mgr.ParseBandwidthGB(r.FormValue("bandwidth"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	if _, err := s.mgr.UpdateQuotasAndBandwidth(name, cpu, memMB, diskGB, bandwidthGB); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	// A pool address is offered on this form only for a container that has none:
	// an address, once handed to a customer, is theirs until the account is
	// deleted, so this can give one but never take one away. "keep" is the
	// no-op choice, "auto" the first free address.
	if sel := strings.TrimSpace(r.FormValue("ipv6")); sel != "" && sel != "keep" {
		want := sel
		if sel == "auto" {
			want = ""
		}
		if _, err := s.mgr.AssignPoolIPv6(name, want); err != nil {
			s.redirect(w, r, s.p(""), "error: "+err.Error())
			return
		}
		_ = s.db.AddAuditLog("000+"+name, "ipv6.assign")
	}
	// A whole /64 can be handed to a container that has none, and only then: a
	// block, once assigned, is the account's for life — this dialog can never
	// take it back (deleting the account is the only way to release it). The
	// manager call is idempotent, so a re-submit is harmless.
	if strings.TrimSpace(r.FormValue("extra64")) != "" {
		if _, err := s.mgr.AssignExtraBlock(name); err != nil {
			s.redirect(w, r, s.p(""), "error: "+err.Error())
			return
		}
		_ = s.db.AddAuditLog("000+"+name, "ipv6.extra64.assign")
	}
	_ = s.db.AddAuditLog("000+"+name, "quota.update")
	s.redirect(w, r, s.p(""), s.t(r, "quota_updated", name))
}

// handleUserExpiry extends (or clears) a user's quota validity. It is the one
// per-user mutation still allowed on an expired account, alongside delete, so
// it is deliberately NOT wrapped in requireTargetActive. The dropdown choices
// map to durations; extension is max(now, current) + duration.
func (s *Server) handleUserExpiry(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := r.FormValue("name")
	switch choice := r.FormValue("extend"); choice {
	case "", "none":
		s.redirect(w, r, s.p(""), s.t(r, "quota_updated", name))
		return
	case "forever":
		if _, err := s.mgr.SetExpiry(name, ""); err != nil {
			s.redirect(w, r, s.p(""), "error: "+err.Error())
			return
		}
	default:
		var d time.Duration
		switch choice {
		case "72h":
			d = 72 * time.Hour
		case "30d":
			d = 30 * 24 * time.Hour
		case "90d":
			d = 90 * 24 * time.Hour
		case "180d":
			d = 180 * 24 * time.Hour
		case "custom":
			n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("days")))
			if err != nil || n <= 0 {
				s.redirect(w, r, s.p(""), "error: "+s.t(r, "err_invalid_extend"))
				return
			}
			d = time.Duration(n) * 24 * time.Hour
		default:
			s.redirect(w, r, s.p(""), "error: "+s.t(r, "err_invalid_extend"))
			return
		}
		if _, err := s.mgr.ExtendExpiryFor(name, d); err != nil {
			s.redirect(w, r, s.p(""), "error: "+err.Error())
			return
		}
	}
	_ = s.db.AddAuditLog("000+"+name, "quota.expiry")
	s.redirect(w, r, s.p(""), s.t(r, "quota_expiry_updated", name))
}

// handleUserBandwidthReset zeroes a user's monthly traffic counters without
// touching the resource quotas. If the user was over quota (and thus
// throttled), the reset also lifts the NIC rate limit immediately.
func (s *Server) handleUserBandwidthReset(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := r.FormValue("name")
	if err := s.mgr.ResetBandwidth(name); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000+"+name, "bandwidth.reset")
	s.redirect(w, r, s.p(""), s.t(r, "bandwidth_reset", name))
}

// handleUserColor sets or clears a user's accent color. Empty color = reset
// to default; a non-empty color must come from the fixed palette (allowlist,
// never a free-form value). It is triggered from the username in the user
// table — a deliberately low-key entry point, documented rather than surfaced
// as its own button.
func (s *Server) handleUserColor(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := r.FormValue("name")
	color := strings.TrimSpace(r.FormValue("color"))
	if color != "" && !validUserColor(color) {
		s.redirect(w, r, s.p(""), "error: "+s.t(r, "err_invalid_color"))
		return
	}
	u, err := s.db.GetUserByName(name)
	if err != nil {
		s.redirect(w, r, s.p(""), "error: user not found")
		return
	}
	if err := s.mgr.SetGroupColor(u.Name, color); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000+"+name, "color.update")
	if color == "" {
		s.redirect(w, r, s.p(""), s.t(r, "color_reset", name))
		return
	}
	s.redirect(w, r, s.p(""), s.t(r, "color_updated", name))
}

func (s *Server) handlePower(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	action := r.FormValue("action")
	name := r.FormValue("name")
	if err := s.mgr.Power(name, action); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000+"+name, "power."+action)
	s.redirect(w, r, s.p(""), s.t(r, "power_ok", name, action))
}

// handleResetPanelPass resets a user's panel login password and shows it once.
func (s *Server) handleResetPanelPass(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := r.FormValue("name")
	pass, err := s.mgr.ResetPanelPassword(name)
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	panel := s.panelURL(r, "/"+s.cfg.Panel.URLPath)
	_ = s.db.AddAuditLog("000+"+name, "passwd.reset")
	// Carry the username so the one-time modal can offer "log in as this
	// user" right after the reset, same as after creating a user.
	s.redirectModalData(w, r, s.p(""), s.t(r, "new_panel_password", name, pass, panel), name)
}

// handleAdminPass changes the admin panel password (no username). The current
// session is preserved.
func (s *Server) handleAdminPass(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	pass := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")
	if pass != confirm {
		s.redirect(w, r, s.p(""), "error: "+s.t(r, "err_pass_mismatch"))
		return
	}
	switch reason := pw.Validate(pass); reason {
	case pw.RejectTooShort:
		s.redirect(w, r, s.p(""), "error: "+s.t(r, "err_pass_short"))
		return
	case pw.RejectWeak:
		s.redirect(w, r, s.p(""), "error: "+s.t(r, "err_pass_weak"))
		return
	}
	hash, err := pw.Hash(pass)
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	if err := s.db.SetSetting(db.SettingAdminPassHash, hash); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	// Every other admin session is now stale: keep only the one that changed
	// the password, so a stolen/long-open session cannot outlive the rotation.
	if c, err := r.Cookie("vpsmgr_admin_session"); err == nil {
		s.sessions.clearExcept(c.Value)
	}
	_ = s.db.AddAuditLog("000", "admin.passwd")
	s.redirect(w, r, s.p(""), s.t(r, "admin_pass_changed"))
}

// handleCPULimit saves the global dynamic CPU limit rule from the admin panel's
// card. The rule lives in the DB (it is not a config option), which is what the
// long-running enforcement loop reads, so saving applies it on the spot: the
// containers that already qualify are capped now, and switching the rule off (or
// an invalid row) restores every capped container to its normal quota.
func (s *Server) handleCPULimit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	rule, err := cpuRuleFromForm(r)
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	if err := s.mgr.SetCPULimitRule(rule); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	if err := s.mgr.EnforceCPULimits(); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000", "cpu_limit.update")
	// Say what actually happened: with the rule switched off nothing is applied
	// (and anything previously capped was just restored), so claiming it is in
	// effect would be misleading.
	msg := "cpu_limit_saved_off"
	if rule.Enabled {
		msg = "cpu_limit_saved"
	}
	s.redirect(w, r, s.p(""), s.t(r, msg))
}

// cpuRuleFromForm reads the CPU limit card's fields. Ranges are validated by the
// manager (the same check the enforcement loop applies), so a value the panel
// accepts can never produce an out-of-range rule in the DB.
func cpuRuleFromForm(r *http.Request) (mgr.CPULimitRule, error) {
	var err error
	rule := mgr.CPULimitRule{Enabled: r.FormValue("enabled") != ""}
	num := func(field string) int {
		if err != nil {
			return 0
		}
		n, e := strconv.Atoi(strings.TrimSpace(r.FormValue(field)))
		if e != nil {
			err = fmt.Errorf("%s must be a whole number", field)
		}
		return n
	}
	rule.WindowMinutes = num("window_minutes")
	rule.Percent = num("percent")
	hours := num("duration_hours")
	minutes := num("duration_minutes")
	rule.DurationSeconds = hours*3600 + minutes*60
	coresStr := strings.TrimSpace(r.FormValue("cores"))
	cores, cerr := strconv.ParseFloat(coresStr, 64)
	if err == nil && cerr != nil {
		err = fmt.Errorf("cores must be a number")
	}
	if err != nil {
		return rule, err
	}
	rule.CoresX10 = int(math.Round(cores * 10))
	return rule, mgr.ValidateCPULimitRule(rule)
}

// handleLoginAs ("log in as user" / impersonation) creates a user-panel
// session for the given username and hands the browser its cookie, dropping the
// operator straight into that user's panel — regardless of the user's password.
// The admin's own session cookie is separate and untouched, so returning to the
// admin panel works normally; impersonating another user later simply replaces
// the user-panel cookie. The session is flagged impersonated, so the user
// panel attributes its audit events "000+<user>" and shows a banner.
func (s *Server) handleLoginAs(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	// Impersonation hands the browser the user-panel cookie. Refuse when the
	// user panel is disabled: a bare "/" cookie path would otherwise be sent to
	// every request on the host, including the admin panel. (With no URLPath
	// there is nothing to impersonate into anyway.)
	if s.cfg.Panel.URLPath == "" {
		s.redirect(w, r, s.p(""), "error: user panel is disabled")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	u, err := s.db.GetUserByName(name)
	if err != nil {
		s.redirect(w, r, s.p(""), "error: user not found")
		return
	}
	sess, err := s.db.CreateImpersonatedSession(u.ID, s.cfg.Panel.SessionDays)
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	// Set the user panel's own session cookie (same name, path and flags as a
	// real login) so the browser becomes that user. Only the user-panel cookie
	// is written here; the admin cookie is left alone.
	userPrefix := "/" + s.cfg.Panel.URLPath
	http.SetCookie(w, &http.Cookie{
		Name:     "vpsmgr_session",
		Value:    sess.Token,
		Path:     userPrefix,
		MaxAge:   s.cfg.Panel.SessionDays * 86400,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	_ = s.db.AddAuditLog("000+"+u.Name, "session.login")
	http.Redirect(w, r, userPrefix+"/", http.StatusFound)
}

// panelURL renders the user-panel address as the browser sees it: https plus
// the request's own Host (not the configured listen port). This keeps the
// address shown in admin modals correct when the panel is reached through a
// reverse proxy on a custom domain or a different port. The scheme is always
// https — the panel only serves TLS and its session cookie is Secure, so it
// cannot function over plain http regardless of the transport in between.
func (s *Server) panelURL(r *http.Request, prefix string) string {
	return "https://" + r.Host + prefix
}

// parseMem parses a memory string ("512" or "1G") into MiB, mirroring the CLI.
func parseMem(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, strconv.ErrSyntax
	}
	mult := 1
	last := s[len(s)-1]
	switch {
	case last >= '0' && last <= '9':
	case last == 'M' || last == 'm':
		s = s[:len(s)-1]
	case last == 'G' || last == 'g':
		mult = 1024
		s = s[:len(s)-1]
	default:
		return 0, strconv.ErrSyntax
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	n *= mult
	if n < 64 {
		return 0, strconv.ErrSyntax
	}
	return n, nil
}

// ---- domain management ----

// domainView is one row of the admin domain panel.
type domainView struct {
	Domain        string
	Username      string
	UpdatedAt     string // UTC RFC3339; rendered in the browser's timezone
	ProxyProtocol bool
}

type domainsPageData struct {
	Title   string
	Prefix  string
	Msg     string
	Err     string
	Domains []domainView
	Blocked string // blocked-domains list, one domain per line (textarea)
	Lang    string
}

func (s *Server) renderDomains(w http.ResponseWriter, r *http.Request, d domainsPageData) {
	t, err := s.templates()
	if err != nil {
		http.Error(w, "template error: "+err.Error(), 500)
		return
	}
	if d.Lang == "" {
		d.Lang = langEn
		if l, ok := r.Context().Value(langCtxKey).(string); ok && l != "" {
			d.Lang = l
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := t.ExecuteTemplate(w, "admin_domains.html", d); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// handleDomains renders the admin domain panel: every domain with its owner
// and last-modified time, newest first, plus the blocked-domains list.
func (s *Server) handleDomains(w http.ResponseWriter, r *http.Request) {
	d := domainsPageData{Title: "VPS Manager Admin — Domains", Prefix: s.prefix()}
	all, err := s.mgr.AllDomains()
	if err != nil {
		d.Err = err.Error()
	} else {
		for _, x := range all {
			d.Domains = append(d.Domains, domainView{Domain: x.Domain, Username: x.Username, UpdatedAt: x.UpdatedAt, ProxyProtocol: x.ProxyProtocol})
		}
	}
	if blocked, err := s.db.GetBlockedDomains(); err != nil {
		if d.Err != "" {
			d.Err += " "
		}
		d.Err += err.Error()
	} else {
		d.Blocked = strings.Join(blocked, "\n")
	}
	s.renderDomains(w, r, d)
}

// maxKnowledgeBytes caps one article's Markdown source (the whole knowledge
// base is embedded in the user panel, so keep it modest).
const maxKnowledgeBytes = 256 << 10

// knowledgeRow is one article in the admin knowledge-base list.
type knowledgeRow struct {
	ID        int64
	Title     string
	UpdatedAt string
}

type knowledgePageData struct {
	Title    string
	Prefix   string
	Msg      string
	Err      string
	Lang     string
	Articles []knowledgeRow
	// Editing is the article currently open in the editor (nil = new one).
	Editing *knowledgeRow
	// Content is the Markdown source shown in the editor.
	Content string
}

func (s *Server) renderKnowledge(w http.ResponseWriter, r *http.Request, d knowledgePageData) {
	t, err := s.templates()
	if err != nil {
		http.Error(w, "template error: "+err.Error(), 500)
		return
	}
	if d.Lang == "" {
		d.Lang = langEn
		if l, ok := r.Context().Value(langCtxKey).(string); ok && l != "" {
			d.Lang = l
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := t.ExecuteTemplate(w, "admin_knowledge.html", d); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// handleKnowledge renders the knowledge-base page: every article plus the
// editor (prefilled via ?edit=<id>).
func (s *Server) handleKnowledge(w http.ResponseWriter, r *http.Request) {
	d := knowledgePageData{Title: "VPS Manager Admin — Knowledge base", Prefix: s.prefix()}
	all, err := s.db.ListKnowledge()
	if err != nil {
		d.Err = err.Error()
	}
	for _, k := range all {
		d.Articles = append(d.Articles, knowledgeRow{ID: k.ID, Title: k.Title, UpdatedAt: k.UpdatedAt})
	}
	if id, err := strconv.ParseInt(r.URL.Query().Get("edit"), 10, 64); err == nil && id > 0 {
		if k, err := s.db.GetKnowledge(id); err == nil && k != nil {
			d.Editing = &knowledgeRow{ID: k.ID, Title: k.Title, UpdatedAt: k.UpdatedAt}
			d.Content = k.Content
		}
	}
	s.renderKnowledge(w, r, d)
}

// handleKnowledgeSave creates (id empty/0) or updates an article.
func (s *Server) handleKnowledgeSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	id, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
	title := strings.TrimSpace(r.FormValue("title"))
	content := strings.TrimSpace(r.FormValue("content"))
	if title == "" {
		s.redirect(w, r, s.p("/knowledge"), "error: "+s.t(r, "err_kb_title"))
		return
	}
	if content == "" {
		s.redirect(w, r, s.p("/knowledge"), "error: "+s.t(r, "err_kb_content"))
		return
	}
	if len(content) > maxKnowledgeBytes {
		s.redirect(w, r, s.p("/knowledge"), "error: "+s.t(r, "err_kb_too_large"))
		return
	}
	if id > 0 {
		if err := s.db.UpdateKnowledge(id, title, content); err != nil {
			s.redirect(w, r, s.p("/knowledge"), "error: "+err.Error())
			return
		}
		_ = s.db.AddAuditLog("000", "knowledge.update")
		s.redirect(w, r, s.p("/knowledge")+"?edit="+strconv.FormatInt(id, 10), s.t(r, "knowledge_saved"))
		return
	}
	newID, err := s.db.CreateKnowledge(title, content)
	if err != nil {
		s.redirect(w, r, s.p("/knowledge"), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000", "knowledge.create")
	s.redirect(w, r, s.p("/knowledge")+"?edit="+strconv.FormatInt(newID, 10), s.t(r, "knowledge_saved"))
}

// handleKnowledgeDel deletes an article.
func (s *Server) handleKnowledgeDel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	id, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
	if err := s.db.DeleteKnowledge(id); err != nil {
		s.redirect(w, r, s.p("/knowledge"), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000", "knowledge.delete")
	s.redirect(w, r, s.p("/knowledge"), s.t(r, "knowledge_deleted"))
}

// handleKnowledgePreview renders Markdown to HTML for the editor's live
// preview. The response is JSON: {"html": "..."}.
func (s *Server) handleKnowledgePreview(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	// The editor sends its FormData body as multipart/form-data, which plain
	// ParseForm ignores (it only reads urlencoded bodies) — the preview then
	// rendered the empty string. ParseMultipartForm parses both, and returns
	// ErrNotMultipart for a urlencoded body, which is fine.
	if err := r.ParseMultipartForm(1 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		HTML template.HTML `json:"html"`
	}{markdown.Render(r.FormValue("content"))})
}

// handleDomainDel deletes a domain (admin path). It finds the owning user and
// removes the domain and its published route atomically.
func (s *Server) handleDomainDel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	domain := r.FormValue("domain")
	if err := s.mgr.AdminDelDomain(domain); err != nil {
		s.redirect(w, r, s.p("/domains"), "error: "+err.Error())
		return
	}
	if dmn, err := s.db.GetDomainByDomain(domain); err == nil {
		if owner, err := s.db.GetUserByID(dmn.UserID); err == nil {
			_ = s.db.AddAuditLog("000+"+owner.Name, "domain.delete")
		}
	}
	s.redirect(w, r, s.p("/domains"), s.t(r, "domain_deleted", domain))
}

// handleDomainUpdate applies the admin batch PROXY protocol toggle, same
// semantics as the user panel.
func (s *Server) handleDomainUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	checked := map[string]bool{}
	for _, d := range r.Form["proto"] {
		checked[d] = true
	}
	all, err := s.mgr.AllDomains()
	if err != nil {
		s.redirect(w, r, s.p("/domains"), "error: "+err.Error())
		return
	}
	changed := 0
	for _, x := range all {
		on := checked[x.Domain]
		if on != x.ProxyProtocol {
			if err := s.mgr.AdminSetDomainProtocol(x.Domain, on); err != nil {
				s.redirect(w, r, s.p("/domains"), "error: "+err.Error())
				return
			}
			_ = s.db.AddAuditLog("000+"+x.Username, "domain_update")
			changed++
		}
	}
	if changed == 0 {
		s.redirect(w, r, s.p("/domains"), "ok: no changes")
		return
	}
	s.redirect(w, r, s.p("/domains"), s.t(r, "domains_updated"))
}

// handleBlockedDomains saves the admin blocked-domains list from the textarea.
// Every line is validated individually: invalid lines are skipped (and
// reported by their 1-based line numbers in a flash banner), the valid lines
// are saved. The check lives in mgr.AddDomain, so all add paths are covered.
func (s *Server) handleBlockedDomains(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	valid, bad := mgr.ParseBlockedList(r.FormValue("blocked"))
	if err := s.db.SetBlockedDomains(valid); err != nil {
		s.redirect(w, r, s.p("/domains"), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000", "blocked.update")
	if len(bad) > 0 {
		lines := make([]string, 0, len(bad))
		for _, n := range bad {
			lines = append(lines, strconv.Itoa(n))
		}
		s.redirect(w, r, s.p("/domains"), s.t(r, "blocked_skipped_lines", strings.Join(lines, ", ")))
		return
	}
	s.redirect(w, r, s.p("/domains"), s.t(r, "blocked_saved"))
}

// ---- IPv6 pool management ----

type ipv6PoolPageData struct {
	Title  string
	Prefix string
	Msg    string
	Err    string
	Lang   string
	Mode   string // "pool" when pool mode is active, else ""
	Total  int
	Used   int
	Free   int
	Addrs  []mgr.PoolEntries
	// Whole /64 blocks (prefix mode): the extra prefix the operator configured
	// ("" = feature off), how its blocks are distributed, and who holds them.
	// Only the ASSIGNED blocks are listed — a /48 would have 65536 of them.
	ExtraPrefix   string
	ExtraTotal    int
	ExtraReserved int
	ExtraUsed     int
	ExtraFree     int
	ExtraAssigned []mgr.ExtraEntry
}

func (s *Server) renderIPv6Pool(w http.ResponseWriter, r *http.Request, d ipv6PoolPageData) {
	t, err := s.templates()
	if err != nil {
		http.Error(w, "template error: "+err.Error(), 500)
		return
	}
	if d.Lang == "" {
		d.Lang = langEn
		if l, ok := r.Context().Value(langCtxKey).(string); ok && l != "" {
			d.Lang = l
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := t.ExecuteTemplate(w, "admin_ipv6pool.html", d); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// handleIPv6Pool renders the pool management page (list + add box).
func (s *Server) handleIPv6Pool(w http.ResponseWriter, r *http.Request) {
	d := ipv6PoolPageData{Title: "VPS Manager Admin — IPv6 Pool", Prefix: s.prefix()}
	if s.mgr.IPv6Mode() == cfg.IPv6ModePool {
		d.Mode = cfg.IPv6ModePool
	} else if s.mgr.IPv6Mode() == cfg.IPv6ModePrefix {
		d.Mode = cfg.IPv6ModePrefix
	} else {
		d.Mode = cfg.IPv6ModeNone
	}
	total, used, err := s.mgr.IPv6PoolUsage()
	if err != nil {
		d.Err = err.Error()
	} else {
		d.Total, d.Used = total, used
		d.Free = total - used
	}
	addrs, err := s.mgr.PoolList()
	if err != nil {
		d.Err = d.Err + " " + err.Error()
	} else {
		d.Addrs = addrs
	}
	// Whole-/64 blocks live on the same page: prefix and none modes have no address pool,
	// so this card is what those modes' page actually shows.
	if d.Mode == cfg.IPv6ModePrefix || d.Mode == cfg.IPv6ModeNone {
		d.ExtraPrefix = s.cfg.Net.IPv6ExtraPrefix
		if t, reserved, u, free, err := s.mgr.ExtraCapacity(); err == nil {
			d.ExtraTotal, d.ExtraReserved, d.ExtraUsed, d.ExtraFree = t, reserved, u, free
		} else {
			d.Err = d.Err + " " + err.Error()
		}
		d.ExtraAssigned = s.mgr.ExtraAssignments()
	}
	s.renderIPv6Pool(w, r, d)
}

// handleIPv6ExtraSet stores net.ipv6_extra_prefix: the optional prefix whole /64
// blocks are carved from. Empty turns the feature off. The value is taken as
// the operator enters it — they assert the prefix is theirs, so nothing checks
// that the provider really routes it — and only the basic shape (a global CIDR
// carrying a length) is validated, by the same registry entry the CLI uses.
func (s *Server) handleIPv6ExtraSet(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f := cfg.FieldFor("net.ipv6_extra_prefix")
	if f == nil {
		s.redirect(w, r, s.p("/ipv6pool"), "error: unknown setting")
		return
	}
	if err := f.Assign(s.cfg, r.FormValue("prefix")); err != nil {
		s.redirect(w, r, s.p("/ipv6pool"), "error: "+err.Error())
		return
	}
	if err := cfg.Save(s.cfg); err != nil {
		s.redirect(w, r, s.p("/ipv6pool"), "error: "+err.Error())
		return
	}
	// Best-effort host plumbing refresh so the change takes effect at once.
	// Existing blocks keep working either way: their routes are restored by the
	// same pass, and changing the prefix never renumbers a container.
	_ = s.mgr.RewireAllIPv6()
	_ = s.db.AddAuditLog("000", "ipv6.extra_prefix")
	s.redirect(w, r, s.p("/ipv6pool"), s.t(r, "extra_prefix_saved"))
}

// handleIPv6PoolAdd batch-adds addresses from the multi-line textarea.
func (s *Server) handleIPv6PoolAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	raw := r.FormValue("addresses")
	var entries []string
	for _, line := range strings.FieldsFunc(raw, func(c rune) bool { return c == '\n' || c == ',' }) {
		line = strings.TrimSpace(line)
		if line != "" {
			entries = append(entries, line)
		}
	}
	added, err := s.mgr.AddPoolIPv6s(entries)
	if err != nil {
		s.redirect(w, r, s.p("/ipv6pool"), "error: "+err.Error())
		return
	}
	if len(added) == 0 {
		s.redirect(w, r, s.p("/ipv6pool"), "ok: no new addresses")
		return
	}
	_ = s.db.AddAuditLog("000", "ipv6pool.add")
	s.redirect(w, r, s.p("/ipv6pool"), s.t(r, "pool_added", len(added)))
}

// handleIPv6PoolDel removes one address from the pool.
func (s *Server) handleIPv6PoolDel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	addr := strings.TrimSpace(r.FormValue("addr"))
	if err := s.mgr.RemovePoolIPv6(addr); err != nil {
		s.redirect(w, r, s.p("/ipv6pool"), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000", "ipv6pool.del")
	s.redirect(w, r, s.p("/ipv6pool"), s.t(r, "pool_removed", addr))
}

// ---- audit log ----

type auditPageData struct {
	Title  string
	Prefix string
	Lang   string
}

func (s *Server) renderAudit(w http.ResponseWriter, r *http.Request, d auditPageData) {
	t, err := s.templates()
	if err != nil {
		http.Error(w, "template error: "+err.Error(), 500)
		return
	}
	if d.Lang == "" {
		d.Lang = langEn
		if l, ok := r.Context().Value(langCtxKey).(string); ok && l != "" {
			d.Lang = l
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := t.ExecuteTemplate(w, "admin_audit.html", d); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// handleAudit renders the audit page shell; rows are fetched chunk-by-chunk by
// the browser from /audit/api so the page never renders thousands of rows.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	s.renderAudit(w, r, auditPageData{Title: "VPS Manager Admin — Audit", Prefix: s.prefix()})
}

// handleAuditAPI returns one chunk of audit rows as JSON for client-side
// rendering (500 per chunk, newest first).
func (s *Server) handleAuditAPI(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	limit := 500
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 1000 {
		limit = l
	}
	rows, err := s.db.ListAuditLog(offset, limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	total, _ := s.db.AuditCount()
	type auditRowJSON struct {
		ID        int64  `json:"id"`
		Actor     string `json:"actor"`
		Action    string `json:"action"`
		CreatedAt string `json:"created_at"`
	}
	out := make([]auditRowJSON, 0, len(rows))
	for _, a := range rows {
		out = append(out, auditRowJSON{a.ID, a.Actor, a.Action, a.CreatedAt})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Rows  []auditRowJSON `json:"rows"`
		More  bool           `json:"more"`
		Total int            `json:"total"`
	}{out, offset+len(rows) < total, total})
}

// handleAdminKeys reconciles the operator's SSH-key store from the management
// panel. The body is a JSON array of key rows (ID > 0 updates, ID == 0 adds,
// missing rows are deleted), same contract as the user panel's /ssh-keys.
// Returns the fresh key list so the panel can re-render without a reload.
func (s *Server) handleAdminKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Keys []mgr.SSHKeyInput `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAdminKeys(w, false, "", nil)
		return
	}
	keys, err := s.mgr.SaveAdminKeys(req.Keys)
	if err != nil {
		writeAdminKeys(w, false, err.Error(), nil)
		return
	}
	_ = s.db.AddAuditLog("000", "admin.keys")
	writeAdminKeys(w, true, "", keys)
}

func writeAdminKeys(w http.ResponseWriter, ok bool, errMsg string, keys []db.AdminKey) {
	rows := make([]sshKeyRow, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, sshKeyRow{ID: k.ID, Name: k.Name, Key: k.Key, Active: k.Active})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		OK    bool        `json:"ok"`
		Error string      `json:"error,omitempty"`
		Keys  []sshKeyRow `json:"keys"`
	}{ok, errMsg, rows})
}
