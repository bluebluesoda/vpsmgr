package lx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Client talks to the local Incus daemon over its Unix socket using the REST
// API. All mutations go through async operations that are waited on, so the
// caller sees the same blocking semantics the `incus` CLI used to provide, but
// without paying for a process spawn on every call.
//
// Exec is implemented over the API websocket transport (the `/1.0/instances/
// <name>/exec` endpoint), so the panel never shells out to the `incus` CLI.
type Client struct {
	base   string
	socket string
	http   *http.Client

	// devLocks serialize device-map read-modify-write PATCHes per instance.
	// SetDisk, EnsureEth0Options and EnsureNicRateLimit each read the full
	// devices map and PATCH it back as a whole; without a per-instance lock
	// two concurrent updates would clobber each other's changes (review P1-5).
	// The lock is held for the whole read-modify-write, so the snapshot a
	// caller modifies is the one it patches.
	devLocks   map[string]*sync.Mutex
	devLocksMu sync.Mutex
}

// New creates a client for the Incus Unix socket at path. The connection is
// lazy: nothing dials the socket until the first request.
func New(socket string) *Client {
	t := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	// Per-request cap guards against a hung daemon. The operation wait uses
	// timeout=30 server-side, so 60s covers it with margin.
	return &Client{
		base:     "http://unix",
		socket:   socket,
		http:     &http.Client{Transport: t, Timeout: 60 * time.Second},
		devLocks: map[string]*sync.Mutex{},
	}
}

// lockDev returns the per-instance device-update lock, creating it on first
// use. The map of locks is itself guarded; callers hold the returned mutex
// for the whole read-modify-write and release it with Unlock (the entry stays
// in the map so a waiting goroutine is guaranteed to block on the SAME mutex).
func (c *Client) lockDev(name string) *sync.Mutex {
	c.devLocksMu.Lock()
	defer c.devLocksMu.Unlock()
	l, ok := c.devLocks[name]
	if !ok {
		l = &sync.Mutex{}
		c.devLocks[name] = l
	}
	return l
}

// dialer returns a websocket dialer that talks to the same Unix socket.
func (c *Client) dialer() *websocket.Dialer {
	return &websocket.Dialer{
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", c.socket)
		},
	}
}

// ---- Incus REST API primitives ----

type response struct {
	Type       string          `json:"type"`
	StatusCode int             `json:"status_code"`
	Error      string          `json:"error"`
	Operation  string          `json:"operation"`
	Metadata   json.RawMessage `json:"metadata"`
}

// do sends a request and returns the Incus response envelope. The envelope's
// "error" type is turned into a Go error.
func (c *Client) do(method, path string, body any) (*response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("incus %s %s: bad response: %w", method, path, err)
	}
	if r.Type == "error" {
		return nil, fmt.Errorf("incus %s %s: %s", method, path, r.Error)
	}
	return &r, nil
}

// get performs a sync GET and unmarshals the metadata into out.
func (c *Client) get(path string, out any) error {
	r, err := c.do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(r.Metadata, out); err != nil {
			return err
		}
	}
	return nil
}

// patch sends a sync PATCH with the given body (unmarshaled into out).
func (c *Client) patch(path string, body, out any) error {
	r, err := c.do(http.MethodPatch, path, body)
	if err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(r.Metadata, out); err != nil {
			return err
		}
	}
	return nil
}

// sendOp triggers an async operation (POST/PUT/DELETE) and waits for it.
// path is the API path; the operation location comes back in the response.
func (c *Client) sendOp(method, path string, body any, timeout time.Duration) error {
	r, err := c.do(method, path, body)
	if err != nil {
		return err
	}
	return c.wait(r.Operation, timeout)
}

// wait blocks until the async operation at opPath (/1.0/operations/...) has
// finished, returning its error.
func (c *Client) wait(opPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var op struct {
			Status string `json:"status"`
			Err    string `json:"err"`
		}
		if err := c.get(opPath+"/wait?timeout=30", &op); err != nil {
			return err
		}
		if op.Status != "Running" {
			if op.Err != "" {
				return errors.New(op.Err)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("incus operation timed out after %v", timeout)
		}
	}
}

// ---- API payload shapes (subset of the Incus API) ----

type instance struct {
	Name    string            `json:"name"`
	Status  string            `json:"status"`
	Config  map[string]string `json:"config"`
	Devices map[string]device `json:"devices"`
}

type device map[string]string

type instState struct {
	Status string `json:"status"`
	CPU    *struct {
		Usage int64 `json:"usage"`
	} `json:"cpu"`
	Memory *struct {
		Usage int64 `json:"usage"`
		Total int64 `json:"total"`
	} `json:"memory"`
	Processes int64 `json:"processes"`
	Network map[string]struct {
		Addresses []struct {
			Family  string `json:"family"`
			Address string `json:"address"`
			Scope   string `json:"scope"`
		} `json:"addresses"`
		Counters struct {
			BytesReceived int64 `json:"bytes_received"`
			BytesSent     int64 `json:"bytes_sent"`
		} `json:"counters"`
	} `json:"network"`
}

type createReq struct {
	Name     string            `json:"name"`
	Source   map[string]string `json:"source"`
	Config   map[string]string `json:"config"`
	Devices  map[string]device `json:"devices"`
	Profiles []string          `json:"profiles"`
}

type stateAction struct {
	Action  string `json:"action"`
	Force   bool   `json:"force,omitempty"`
	Timeout int    `json:"timeout,omitempty"`
}

// execReq is the body of a POST /1.0/instances/<name>/exec call. wait-for-websocket
// makes the operation respond with the fds (websocket secrets) in its metadata.
type execReq struct {
	Command            []string          `json:"command"`
	WaitForWebsocket   bool              `json:"wait-for-websocket"`
	Interactive        bool              `json:"interactive"`
	Environment        map[string]string `json:"environment,omitempty"`
	RecordOutput       bool              `json:"record-output,omitempty"`
	User               uint32            `json:"user,omitempty"`
	Group              uint32            `json:"group,omitempty"`
	Cwd                string            `json:"cwd,omitempty"`
	Width              int               `json:"width,omitempty"`
	Height             int               `json:"height,omitempty"`
}

// ---- high-level helpers ----

func (c *Client) list() ([]instance, error) {
	var insts []instance
	if err := c.get("/1.0/instances?recursion=1", &insts); err != nil {
		return nil, err
	}
	return insts, nil
}

// stateOf returns the live state of one instance.
func (c *Client) stateOf(name string) (*instState, error) {
	var st instState
	if err := c.get("/1.0/instances/"+url.PathEscape(name)+"/state", &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// containerInfo maps a live state into the ContainerInfo shape.
func containerInfo(name string, st *instState) ContainerInfo {
	ci := ContainerInfo{Status: st.Status}
	if st.CPU != nil {
		ci.CPUUsage = st.CPU.Usage
	}
	if st.Memory != nil {
		ci.MemUsage = st.Memory.Usage
		ci.MemTotal = st.Memory.Total
	}
	ci.Processes = st.Processes
	for _, ifs := range st.Network {
		ci.Rx += ifs.Counters.BytesReceived
		ci.Tx += ifs.Counters.BytesSent
		for _, a := range ifs.Addresses {
			if ci.IPv4 == "" && a.Family == "inet" && a.Address != "127.0.0.1" {
				ci.IPv4 = a.Address
			}
		}
	}
	return ci
}

// snapshot returns the status plus live state of every instance using ONE list
// call followed by concurrent per-instance state calls for the running ones.
func (c *Client) snapshot() (map[string]ContainerInfo, error) {
	insts, err := c.list()
	if err != nil {
		return nil, err
	}
	out := make(map[string]ContainerInfo, len(insts))
	type res struct {
		name string
		ci   ContainerInfo
	}
	ch := make(chan res, len(insts))
	running := 0
	for _, it := range insts {
		out[it.Name] = ContainerInfo{Status: it.Status}
		if it.Status != "Running" {
			continue
		}
		running++
		go func(n string) {
			st, err := c.stateOf(n)
			if err != nil {
				ch <- res{} // keep the Running status we already recorded
				return
			}
			ch <- res{n, containerInfo(n, st)}
		}(it.Name)
	}
	for i := 0; i < running; i++ {
		if r := <-ch; r.name != "" {
			out[r.name] = r.ci
		}
	}
	return out, nil
}

// ---- data types exposed to mgr ----

// Usage describes a container's live CPU/memory accounting.
type Usage struct {
	CPUUsage int64 // nanoseconds of CPU time used since start
	MemUsage int64 // bytes currently used
	MemTotal int64 // memory limit in bytes
}

// UsageMap returns current CPU/memory accounting for every container.
func (c *Client) UsageMap() (map[string]Usage, error) {
	sn, err := c.snapshot()
	if err != nil {
		return nil, err
	}
	m := make(map[string]Usage, len(sn))
	for name, ci := range sn {
		if ci.Status != "Running" {
			continue
		}
		m[name] = Usage{CPUUsage: ci.CPUUsage, MemUsage: ci.MemUsage, MemTotal: ci.MemTotal}
	}
	return m, nil
}

// Bandwidth describes a container's cumulative network counters since its last
// start. Counters are per-device and reset to zero when the container is
// restarted or reinstalled.
type Bandwidth struct {
	Rx int64 // bytes received (download)
	Tx int64 // bytes sent (upload)
}

// BandwidthMap returns the cumulative network counters of every running
// container, keyed by container name. Stopped containers have no state and are
// omitted.
func (c *Client) BandwidthMap() (map[string]Bandwidth, error) {
	sn, err := c.snapshot()
	if err != nil {
		return nil, err
	}
	m := make(map[string]Bandwidth, len(sn))
	for name, ci := range sn {
		if ci.Status != "Running" {
			continue
		}
		m[name] = Bandwidth{Rx: ci.Rx, Tx: ci.Tx}
	}
	return m, nil
}

// ContainerInfo is one snapshot of a container taken from a single state read.
type ContainerInfo struct {
	Status     string
	CPUUsage   int64 // nanoseconds of CPU time since start (0 if not running)
	MemUsage   int64 // bytes currently used (0 if not running)
	MemTotal   int64 // memory limit in bytes (0 if not running)
	Processes  int64 // number of processes inside the container (0 if not running)
	Rx         int64 // cumulative bytes received (download) since start
	Tx         int64 // cumulative bytes sent (upload) since start
	IPv4       string
}

// Containers returns a live snapshot of every container: one list call plus
// concurrent state reads, regardless of the number of containers. Stopped
// containers yield zeroed CPU/mem/bandwidth values with Status reflecting the
// real status.
func (c *Client) Containers() (map[string]ContainerInfo, error) {
	return c.snapshot()
}

// State returns the status of one container.
func (c *Client) State(name string) (string, error) {
	var it instance
	if err := c.get("/1.0/instances/"+url.PathEscape(name), &it); err != nil {
		return "", err
	}
	return it.Status, nil
}

// ---- mutations ----

// NetworkSet sets one key=value config option on a managed Incus network
// (e.g. incusbr0). Used for IPv6 pass-through bridge configuration.
func (c *Client) NetworkSet(network, kv string) error {
	key, val, ok := strings.Cut(kv, "=")
	if !ok {
		return fmt.Errorf("incus network set: invalid key=value %q", kv)
	}
	body := map[string]map[string]string{"config": {key: val}}
	return c.patch("/1.0/networks/"+url.PathEscape(network), body, nil)
}

func (c *Client) Start(name string) error {
	return c.sendOp(http.MethodPut, "/1.0/instances/"+url.PathEscape(name)+"/state",
		stateAction{Action: "start"}, 2*time.Minute)
}

func (c *Client) Stop(name string) error {
	return c.sendOp(http.MethodPut, "/1.0/instances/"+url.PathEscape(name)+"/state",
		stateAction{Action: "stop"}, 2*time.Minute)
}

func (c *Client) Restart(name string) error {
	if err := c.sendOp(http.MethodPut, "/1.0/instances/"+url.PathEscape(name)+"/state",
		stateAction{Action: "restart"}, 2*time.Minute); err != nil {
		return err
	}
	return c.WaitReady(name, 90*time.Second)
}

// DefaultProcessesLimit is the per-container process (pids.max) cap applied to
// every new container. It stops one container's fork storm from exhausting the
// host's PID space (kernel.threads-max) and DoSing every tenant. Hardcoded for
// now — no config knob; the admin panel renders it as "<used> / 4096".
const DefaultProcessesLimit = "4096"

// cpuLimitConfig maps a CPU quota in tenths of a core onto Incus config keys.
// Whole cores set `limits.cpu=<n>`. Fractional quotas (0.1..0.9) pin the
// container to a single core and add a time allowance
// (`limits.cpu.allowance=<n>ms/100ms`) so it may only use that slice of the
// core. Setting the allowance to "" removes it, which is how a fractional
// quota is switched back to whole cores (PATCH merges and deletes empty keys).
func cpuLimitConfig(cpuTenths int) map[string]string {
	if cpuTenths%10 != 0 {
		return map[string]string{
			"limits.cpu":           "1",
			"limits.cpu.allowance": strconv.Itoa(cpuTenths*10) + "ms/100ms",
		}
	}
	return map[string]string{
		"limits.cpu":           strconv.Itoa(cpuTenths / 10),
		"limits.cpu.allowance": "",
	}
}

// SetCPU live-updates the CPU quota (tenths of a core). Whole cores set
// `limits.cpu`; fractional quotas pin to one core with a time allowance.
func (c *Client) SetCPU(name string, cpuTenths int) error {
	return c.patch("/1.0/instances/"+url.PathEscape(name),
		map[string]map[string]string{"config": cpuLimitConfig(cpuTenths)}, nil)
}

// SetAutostart toggles whether the container starts automatically when the
// host boots. Containers stopped via the panel (user or admin) are disabled so
// a maintenance reboot does not bring them back; start/restart re-enable it.
func (c *Client) SetAutostart(name string, on bool) error {
	body := map[string]map[string]string{"config": {"boot.autostart": strconv.FormatBool(on)}}
	return c.patch("/1.0/instances/"+url.PathEscape(name), body, nil)
}

// SetMem live-updates the memory limit.
func (c *Client) SetMem(name string, mb int) error {
	body := map[string]map[string]string{"config": {"limits.memory": strconv.Itoa(mb) + "MiB"}}
	return c.patch("/1.0/instances/"+url.PathEscape(name), body, nil)
}

// SetDisk grows the root device's size. The device map is fetched first and
// patched as a whole because a PATCH replaces the entire devices map. Held
// under the per-instance device lock so a concurrent EnsureNicRateLimit or
// EnsureEth0Options cannot be lost in the PATCH.
func (c *Client) SetDisk(name string, gb int) error {
	l := c.lockDev(name)
	l.Lock()
	defer l.Unlock()
	var it instance
	if err := c.get("/1.0/instances/"+url.PathEscape(name)+"?recursion=1", &it); err != nil {
		return err
	}
	devices := it.Devices
	root, ok := devices["root"]
	if !ok {
		return fmt.Errorf("incus: instance %s has no root device", name)
	}
	root["size"] = strconv.Itoa(gb) + "GiB"
	devices["root"] = root
	body := map[string]map[string]device{"devices": devices}
	return c.patch("/1.0/instances/"+url.PathEscape(name), body, nil)
}

// EnsureEth0Options ensures eth0 carries the given options, patching the
// device and restarting the container when any are missing (preserving a
// stopped state). Returns true when a change was made. Patching a running
// container hot-removes eth0, which trips an Incus netprio bug and can leave
// the option unapplied, so the container is stopped first.
func (c *Client) EnsureEth0Options(name string, opts map[string]string) (bool, error) {
	l := c.lockDev(name)
	l.Lock()
	defer l.Unlock()
	var it instance
	if err := c.get("/1.0/instances/"+url.PathEscape(name)+"?recursion=1", &it); err != nil {
		return false, err
	}
	eth0, ok := it.Devices["eth0"]
	if !ok {
		return false, fmt.Errorf("incus: instance %s has no eth0 device", name)
	}
	changed := false
	for k, v := range opts {
		if eth0[k] != v {
			eth0[k] = v
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	wasRunning := it.Status == "Running"
	if wasRunning {
		if err := c.Stop(name); err != nil {
			return false, err
		}
	}
	body := map[string]map[string]device{"devices": it.Devices}
	if err := c.patch("/1.0/instances/"+url.PathEscape(name), body, nil); err != nil {
		return false, err
	}
	if wasRunning {
		return true, c.Start(name)
	}
	return true, nil
}

// EnsureNicRateLimit sets (rate != "") or clears (rate == "") the eth0
// rate limit of a container. Changing only the limits.* keys is applied
// LIVE by Incus via tc (htb qdisc on the host veth) — it does NOT reset the NIC
// or restart the container. An instance PATCH replaces the entire devices map,
// so the device map is read first and patched as a whole. Safe on running and
// stopped instances.
func (c *Client) EnsureNicRateLimit(name, rate string) error {
	l := c.lockDev(name)
	l.Lock()
	defer l.Unlock()
	var it instance
	if err := c.get("/1.0/instances/"+url.PathEscape(name)+"?recursion=1", &it); err != nil {
		return err
	}
	eth0, ok := it.Devices["eth0"]
	if !ok {
		return fmt.Errorf("incus: instance %s has no eth0 device", name)
	}
	if rate == "" {
		delete(eth0, "limits.ingress")
		delete(eth0, "limits.egress")
	} else {
		eth0["limits.ingress"] = rate
		eth0["limits.egress"] = rate
	}
	body := map[string]map[string]device{"devices": it.Devices}
	return c.patch("/1.0/instances/"+url.PathEscape(name), body, nil)
}

// NicRateLimit returns the eth0 rate limit currently applied to a container,
// or "" when unset. Used after a process restart to rebuild the in-memory
// throttle state from what Incus actually has, so a stale limit is not left on
// a container that is back under quota.
func (c *Client) NicRateLimit(name string) (string, error) {
	var it instance
	if err := c.get("/1.0/instances/"+url.PathEscape(name)+"?recursion=1", &it); err != nil {
		return "", err
	}
	eth0, ok := it.Devices["eth0"]
	if !ok {
		return "", fmt.Errorf("incus: instance %s has no eth0 device", name)
	}
	if r := eth0["limits.egress"]; r != "" {
		return r, nil
	}
	return eth0["limits.ingress"], nil
}

// HardenIsolation ensures a container's eth0 carries the NIC isolation options.
// Idempotent. IPv6 filtering is only applied when eth0 already has an IPv6
// address or route (Incus 7.0 rejects ipv6_filtering without one).
func (c *Client) HardenIsolation(name string) (bool, error) {
	var it instance
	if err := c.get("/1.0/instances/"+url.PathEscape(name)+"?recursion=1", &it); err != nil {
		return false, err
	}
	eth0, ok := it.Devices["eth0"]
	if !ok {
		return false, fmt.Errorf("incus: instance %s has no eth0 device", name)
	}
	iso := nicIsolation
	if eth0["ipv6.address"] == "" && eth0["ipv6.routes"] == "" {
		iso = nicIsolationNoV6
	}
	return c.EnsureEth0Options(name, iso)
}

// Delete force-stops the container if needed and removes it. Already-gone
// containers are treated as success so deletions are retryable after a partial
// cleanup; any other failure (e.g. the daemon being unreachable) is returned
// because the caller must not pretend the container is gone.
func (c *Client) Delete(name string) error {
	st, err := c.stateOf(name)
	if err != nil {
		if strings.Contains(err.Error(), "Instance not found") {
			return nil
		}
		return err
	}
	if st.Status != "Stopped" {
		_ = c.sendOp(http.MethodPut, "/1.0/instances/"+url.PathEscape(name)+"/state",
			stateAction{Action: "stop", Force: true, Timeout: -1}, 2*time.Minute)
	}
	return c.sendOp(http.MethodDelete, "/1.0/instances/"+url.PathEscape(name), nil, 2*time.Minute)
}

// InstanceStaticIPs returns every instance's name and its configured static
// IPv4 (the eth0 device's ipv4.address), regardless of running state. Instances
// created from a profile without an own eth0 override carry an empty IP but are
// still returned, so the caller can detect name collisions too. Used to refuse
// an add whose name or IP is already claimed by a live Incus instance.
func (c *Client) InstanceStaticIPs() (map[string]string, error) {
	var insts []instance
	if err := c.get("/1.0/instances?recursion=1", &insts); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(insts))
	for _, it := range insts {
		ip := ""
		if d, ok := it.Devices["eth0"]; ok {
			ip = d["ipv4.address"]
		}
		out[it.Name] = ip
	}
	return out, nil
}

// ImageExists reports whether an image alias is present.
func (c *Client) ImageExists(alias string) (bool, error) {
	_, err := c.do(http.MethodGet, "/1.0/images/aliases/"+url.PathEscape(alias), nil)
	if err != nil {
		return false, nil
	}
	return true, nil
}

// EnsureImage makes sure an image alias is available locally, pulling it from
// its remote when needed. The alias may be a plain local alias ("debian/13")
// or a remote-qualified one ("images:debian/13"). The remote-qualified form is
// split into server+alias for the pull request (the Incus API does not accept
// the "remote:" prefix in the source.alias field). No-op when the local image
// already exists.
func (c *Client) EnsureImage(alias string) error {
	// Normalize a "remote:alias" form down to the local alias before checking,
	// so an already-cached remote image is not re-pulled.
	local := alias
	if _, name, found := strings.Cut(alias, ":"); found {
		local = name
	}
	if ok, _ := c.ImageExists(local); ok {
		return nil
	}
	body := map[string]any{
		"source": map[string]string{
			"type":     "image",
			"alias":    local,
			"server":   "https://images.linuxcontainers.org",
			"protocol": "simplestreams",
		},
	}
	r, err := c.do(http.MethodPost, "/1.0/images", body)
	if err != nil {
		return err
	}
	return c.wait(r.Operation, 5*time.Minute)
}

// ImageAliases returns every image alias stored locally, e.g. to enumerate the
// managed reinstall images (`vpsmgr/*-sshd`).
func (c *Client) ImageAliases() ([]string, error) {
	var images []struct {
		Aliases []struct {
			Name string `json:"name"`
		} `json:"aliases"`
	}
	if err := c.get("/1.0/images?recursion=1", &images); err != nil {
		return nil, err
	}
	var out []string
	for _, img := range images {
		for _, a := range img.Aliases {
			out = append(out, a.Name)
		}
	}
	return out, nil
}

// PoolResources returns the storage pool's total/used bytes from the Incus API.
func (c *Client) PoolResources(pool string) (total, used int64, err error) {
	var res struct {
		Space struct {
			Used  int64 `json:"used"`
			Total int64 `json:"total"`
		} `json:"space"`
	}
	if err := c.get("/1.0/storage-pools/"+url.PathEscape(pool)+"/resources", &res); err != nil {
		return 0, 0, err
	}
	return res.Space.Total, res.Space.Used, nil
}

// nicIsolation maps to Incus per-NIC security options that isolate a
// container's eth0 from every other container on the bridge:
//
//   - security.port_isolation: the veth is an isolated bridge port, so no
//     frames (unicast, multicast, broadcast) flow between containers at L2 —
//     ARP/NDP spoofing, L2 sniffing and rogue DHCP/DNS servers all die here.
//   - security.ipv4/ipv6_filtering: Incus installs bridge input rules dropping
//     ARP/NDP that claims an address the container doesn't own, protecting the
//     host's own ARP/NDP cache from container-side poisoning.
//
// A side effect of port isolation is that containers can no longer talk to
// each other on the private bridge — by design (see docs/architecture.md).
//
// IPv6 filtering is applied only when the eth0 device carries an IPv6 address
// or route: Incus 7.0 rejects `security.ipv6_filtering=true` on a device with
// no `ipv6.address` when the parent bridge has IPv6 disabled (a validation
// change from LXD, where an empty IPv6 was accepted).
var (
	nicIsolation = map[string]string{
		"security.port_isolation": "true",
		"security.ipv4_filtering": "true",
		"security.ipv6_filtering": "true",
	}
	nicIsolationNoV6 = map[string]string{
		"security.port_isolation": "true",
		"security.ipv4_filtering": "true",
	}
)

// Launch creates a container with limits, static IPv4 (and optional static
// IPv6 primary address + routed /112 block), root size and autostart enabled,
// then starts it and waits until it is ready. security.nesting allows running
// Docker / nested containers inside.
// pool and bridge name the storage pool and managed bridge (from config).
// cpu is a quota in tenths of a core (see cpuLimitConfig).
// Everything is submitted in ONE create request — the config, the eth0 static
// addresses and the root size — so no follow-up device overrides are needed.
func (c *Client) Launch(pool, bridge, name, image, ip, ipv6, block string, cpu, memMB, diskGB int) error {
	// The create source takes a plain local alias; strip a "remote:" prefix
	// (the image is ensured to be cached locally before Launch is called).
	if _, local, found := strings.Cut(image, ":"); found {
		image = local
	}
	eth0 := device{
		"type":         "nic",
		"nictype":      "bridged",
		"parent":       bridge,
		"name":         "eth0",
		"ipv4.address": ip,
	}
	if ipv6 != "" {
		eth0["ipv6.address"] = ipv6
	}
	if block != "" {
		eth0["ipv6.routes"] = block
	}
	// security.ipv6_filtering needs an IPv6 address/route on the device (Incus
	// 7.0 validation); skip it when the container has no IPv6 at all.
	iso := nicIsolation
	if ipv6 == "" && block == "" {
		iso = nicIsolationNoV6
	}
	for k, v := range iso {
		eth0[k] = v
	}
	config := cpuLimitConfig(cpu)
	config["limits.memory"] = strconv.Itoa(memMB) + "MiB"
	config["limits.processes"] = DefaultProcessesLimit
	config["boot.autostart"] = "true"
	config["security.nesting"] = "true"
	req := createReq{
		Name:   name,
		Source: map[string]string{"type": "image", "alias": image},
		Config: config,
		Devices: map[string]device{
			"eth0": eth0,
			"root": {"type": "disk", "path": "/", "pool": pool, "size": strconv.Itoa(diskGB) + "GiB"},
		},
		Profiles: []string{"default"},
	}
	if err := c.sendOp(http.MethodPost, "/1.0/instances", req, 5*time.Minute); err != nil {
		return err
	}
	// Creating an instance leaves it Stopped; start it before waiting.
	if err := c.Start(name); err != nil {
		return err
	}
	return c.WaitReady(name, 120*time.Second)
}

// WaitReady waits until the container is running and accepts exec.
func (c *Client) WaitReady(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, err := c.State(name); err == nil && st == "Running" {
			if _, err := c.ExecSH(name, "true"); err == nil {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("container %s not ready within %v", name, timeout)
}

// ExecSH runs a shell script inside a container as root over the exec API
// (websocket transport), so no `incus` CLI process is ever spawned.
func (c *Client) ExecSH(name, script string) (string, error) {
	out, err := c.Exec(name, []string{"/bin/sh", "-c", script}, "", time.Minute)
	if err != nil {
		return "", fmt.Errorf("incus exec %s: %s", name, strings.TrimSpace(err.Error()))
	}
	return strings.TrimSpace(out), nil
}

// RunInitScript writes a user's custom init script into the container and runs
// it DETACHED, logging to /var/log/vpsmgr-init.log inside the container.
//
// Safety (the script is fully user-controlled and may be hostile):
//   - the script is delivered over stdin to `cat >/root/vpsmgr-init.sh`, never
//     interpolated into the host command line or argv, so it cannot escape the
//     exec; it only ever runs INSIDE the container
//   - a script starting with a shebang is executed directly (the kernel honors
//     #!/bin/bash etc.); otherwise it runs under /bin/sh
//   - the job is backgrounded with nohup and its stdin/stdout/stderr redirected
//     to a file / /dev/null, so a runaway script cannot block the caller (the
//     panel reinstall) — the host exec returns right after spawning it
//   - the exec itself is bounded by a timeout, so even a wedged container
//     cannot hang the call
func (c *Client) RunInitScript(name, script string) error {
	_, err := c.Exec(name, []string{"/bin/sh", "-c", initScriptCmd(script, "/root/vpsmgr-init.sh", "/var/log/vpsmgr-init.log")},
		script, 30*time.Second)
	if err != nil {
		return fmt.Errorf("incus exec %s: %s", name, strings.TrimSpace(err.Error()))
	}
	return nil
}

// Exec runs command inside the container over the exec API websocket transport
// and returns its combined stdout (stderr is folded in on error). stdin, when
// non-empty, is written to the child's stdin before it is closed (EOF).
func (c *Client) Exec(name string, command []string, stdin string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req := execReq{
		Command:          command,
		WaitForWebsocket: true,
		Interactive:      false,
		Environment:      map[string]string{"TERM": "dumb"},
		User:             0,
		Group:            0,
	}

	// POST the exec operation. With wait-for-websocket the response carries the
	// websocket secrets (fds) in the operation metadata.
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/1.0/instances/"+url.PathEscape(name)+"/exec", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var op struct {
		Type       string `json:"type"`
		Operation  string `json:"operation"`
		Error      string `json:"error"`
		StatusCode int    `json:"status_code"`
		Metadata   struct {
			Status string `json:"status"`
			Err    string `json:"err"`
			Inner  struct {
				Fds map[string]string `json:"fds"`
			} `json:"metadata"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &op); err != nil {
		return "", fmt.Errorf("incus exec %s: bad response: %w", name, err)
	}
	if op.Type == "error" {
		return "", errors.New(op.Error)
	}
	if op.Operation == "" {
		return "", fmt.Errorf("incus exec %s: no operation returned", name)
	}
	if len(op.Metadata.Inner.Fds) == 0 {
		return "", fmt.Errorf("incus exec %s: no websocket fds in response", name)
	}
	opID := strings.TrimPrefix(op.Operation, "/1.0/operations/")
	wsURL := func(secret string) string {
		return "ws://unix/1.0/operations/" + opID + "/websocket?secret=" + secret
	}

	dialer := c.dialer()
	conns := make(map[string]*websocket.Conn, len(op.Metadata.Inner.Fds))
	for k, secret := range op.Metadata.Inner.Fds {
		ws, _, err := dialer.DialContext(ctx, wsURL(secret), nil)
		if err != nil {
			return "", fmt.Errorf("incus exec %s: connect fd %s: %w", name, k, err)
		}
		conns[k] = ws
		defer ws.Close()
	}

	stdout, ok1 := conns["1"]
	stderr, ok2 := conns["2"]
	control := conns["control"]
	stdinConn, ok0 := conns["0"]
	if !ok1 || !ok2 {
		return "", fmt.Errorf("incus exec %s: missing stdout/stderr fds", name)
	}

	// Copy stdout and stderr concurrently.
	type stream struct {
		text string
		err  error
	}
	readLoop := func(ws *websocket.Conn) chan stream {
		ch := make(chan stream, 1)
		go func() {
			var sb strings.Builder
			for {
				_, msg, err := ws.ReadMessage()
				if err != nil {
					ch <- stream{sb.String(), err}
					return
				}
				sb.Write(msg)
			}
		}()
		return ch
	}
	stdoutCh := readLoop(stdout)
	stderrCh := readLoop(stderr)
	// Drain the control channel if present so the command can finish.
	if control != nil {
		go func() {
			for {
				if _, _, err := control.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}

	// All fds are connected now, so the command has been spawned. Send stdin
	// (binary frame — Incus copies the raw bytes to the child's stdin) and
	// close the socket to signal EOF. For an empty stdin just closing is the
	// EOF.
	if ok0 {
		if stdin != "" {
			_ = stdinConn.WriteMessage(websocket.BinaryMessage, []byte(stdin))
		}
		_ = stdinConn.Close()
	}

	// Wait for stdout EOF (command finished).
	var outText, errText string
	select {
	case s := <-stdoutCh:
		outText = s.text
	case <-ctx.Done():
		return "", fmt.Errorf("incus exec %s: %v", name, ctx.Err())
	}
	select {
	case s := <-stderrCh:
		errText = s.text
	default:
	}

	// The command has exited: read the operation's metadata.return — the exit
	// status the server recorded via op.ExtendMetadata. Relying on "stdout EOF"
	// or "stderr non-empty" alone misclassifies `sh -c 'exit 1'` (no output,
	// non-zero exit) as success. The websocket control channel is
	// client→server only (signals/resize), so the exit code cannot come from
	// there — it is always on the operation record.
	status := -1
	var opStatus struct {
		Metadata struct {
			Return *int   `json:"return"`
			Err    string `json:"err"`
		} `json:"metadata"`
		StatusCode int `json:"status_code"`
	}
	if err := c.get("/1.0/operations/"+opID, &opStatus); err != nil {
		return "", fmt.Errorf("incus exec %s: read operation status: %w", name, err)
	}
	if opStatus.Metadata.Return != nil {
		status = *opStatus.Metadata.Return
	}
	if status != 0 {
		msg := strings.TrimSpace(errText)
		if msg == "" {
			msg = strings.TrimSpace(outText)
		}
		if msg != "" {
			return "", fmt.Errorf("command exited %d: %s", status, msg)
		}
		return "", fmt.Errorf("command exited %d", status)
	}
	if errText != "" {
		return "", errors.New(strings.TrimSpace(errText))
	}
	return outText, nil
}

// initScriptCmd builds the container-side shell command that delivers script to
// path and runs it detached, logging to logPath.
//
// The write (`cat` + `chmod`) runs in the FOREGROUND and must finish before the
// exec returns. The run is wrapped in a `( ... & )` subshell so ONLY the run is
// backgrounded: a trailing `&` (as in an earlier version) backgrounds the whole
// `cat && chmod && nohup` chain, so sh -c exits before cat has read stdin and
// the session close kills the backgrounded cat mid-write — the file is left
// empty and nothing ever runs.
func initScriptCmd(script, path, logPath string) string {
	run := "nohup sh " + path
	if hasShebang(script) {
		run = "nohup " + path
	}
	return "cat >" + path + " && chmod 700 " + path + " && (" + run +
		" >" + logPath + " 2>&1 </dev/null &)"
}

// hasShebang reports whether a script starts with a #! interpreter line.
func hasShebang(script string) bool {
	line := script
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return strings.HasPrefix(strings.TrimSpace(line), "#!")
}
