package lx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ---- cross-machine container transfer ----
//
// Two long-running operations back `vps transfer`: exporting a stopped
// container's root disk to a single tarball, and importing that tarball as a
// new container on another host. Both move tens of gigabytes, so they use the
// client's deadline-free `stream` HTTP client and never buffer through memory.

// importTimeout bounds the backup import operation. Unpacking a large rootfs
// into the storage pool is the longest step of a transfer by a wide margin, so
// it gets a budget far above the ordinary lifecycle calls.
const importTimeout = 6 * time.Hour

// BackupOptions describes one backup export. InstanceOnly and RootOnly are
// always set by the caller: a transfer moves the container as it is now, not
// the source's snapshot history or its dependent volumes.
type BackupOptions struct {
	// Name is the backup name Incus records for the stream.
	Name string
	// Compression is "none", "gzip", "zstd" or "" (Incus default).
	Compression string
	// Optimized stores a storage-driver native stream instead of a plain
	// tarball. It is far faster but the file can only be restored into a pool
	// using the same driver (zfs to zfs, btrfs to btrfs), so it is opt-in.
	Optimized    bool
	InstanceOnly bool
	RootOnly     bool
}

// Device is one Incus device: a map of properties such as type, path, pool,
// nictype or ipv4.address.
type Device = device

// InstanceSpec exposes the container specification builder (instanceSpec) to
// the manager. A transfer uses it to re-home an imported container onto this
// host's own IP, network layout and quota, replacing whatever the source host
// had recorded in the archive.
func (c *Client) InstanceSpec(pool, bridge, ip, ipv6, block, poolIPv6, extIF string, cpu, memMB, diskGB int) (map[string]string, map[string]Device) {
	return c.instanceSpec(pool, bridge, ip, ipv6, block, poolIPv6, extIF, cpu, memMB, diskGB)
}

// BackupExport streams a backup of the container into w and returns the number
// of bytes written. It is sent with Accept: application/octet-stream, which
// makes Incus stream the tarball directly instead of creating a backup object
// in the pool first — nothing is left behind on the source host.
//
// The caller must have stopped the container: a backup of a running container
// is a file-level copy of a live filesystem and can be inconsistent.
func (c *Client) BackupExport(ctx context.Context, name string, opt BackupOptions, w io.Writer) (int64, error) {
	body := map[string]any{
		"name":              opt.Name,
		"instance_only":     opt.InstanceOnly,
		"root_only":         opt.RootOnly,
		"optimized_storage": opt.Optimized,
	}
	if opt.Compression != "" {
		body["compression_algorithm"] = opt.Compression
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/1.0/instances/"+url.PathEscape(name)+"/backups", bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := c.stream.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, c.streamError("backup export", name, resp)
	}
	return io.Copy(w, resp.Body)
}

// BackupImport creates the container from a backup tarball read from r. Incus
// takes the raw archive as the request body and the target name and pool from
// headers; the created container carries whatever configuration the archive
// holds, so the caller replaces it afterwards (see ReplaceConfig).
//
// Cancelling ctx aborts the upload, and the daemon discards the partial
// instance rather than leaving one behind.
func (c *Client) BackupImport(ctx context.Context, name, pool string, r io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/1.0/instances", r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Incus-name", name)
	if pool != "" {
		req.Header.Set("X-Incus-pool", pool)
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return c.streamError("backup import", name, resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var env response
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("incus backup import %s: bad response: %w", name, err)
	}
	if env.Type == "error" {
		return fmt.Errorf("incus backup import %s: %s", name, env.Error)
	}
	return c.wait(env.Operation, importTimeout)
}

// ReplaceConfig overwrites the named container's config and device map with the
// given ones.
//
// This is how an imported container is re-homed: the archive carries the source
// host's instance configuration (its static IPv4 on eth0, its limits, its root
// device), and all of it has to be replaced by what this host's own spec says.
// Daemon-managed volatile.* keys (the idmap, the UUID, the NIC hardware
// address) are read back from the live instance and carried through unchanged —
// a PUT replaces the config wholesale, so dropping them would make Incus
// regenerate the container's identity and MAC.
func (c *Client) ReplaceConfig(name string, config map[string]string, devices map[string]device) error {
	var cur instance
	if err := c.get("/1.0/instances/"+url.PathEscape(name), &cur); err != nil {
		return err
	}
	merged := make(map[string]string, len(config)+len(cur.Config))
	for k, v := range config {
		merged[k] = v
	}
	for k, v := range cur.Config {
		if strings.HasPrefix(k, "volatile.") {
			merged[k] = v
		}
	}
	profiles := cur.Profiles
	if len(profiles) == 0 {
		profiles = []string{"default"}
	}
	body := struct {
		Architecture string            `json:"architecture"`
		Config       map[string]string `json:"config"`
		Devices      map[string]device `json:"devices"`
		Profiles     []string          `json:"profiles"`
	}{Architecture: cur.Architecture, Config: merged, Devices: devices, Profiles: profiles}
	return c.sendOp(http.MethodPut, "/1.0/instances/"+url.PathEscape(name), body, 2*time.Minute)
}

// DiskUsage returns the number of bytes the container's root volume currently
// occupies, used to size the temp file before an export starts. It reports 0
// when the daemon has no figure to give (a stopped container on some drivers),
// which the caller treats as "unknown" and falls back to a quota-based bound.
func (c *Client) DiskUsage(name string) (int64, error) {
	var st struct {
		Disk map[string]struct {
			Usage int64 `json:"usage"`
		} `json:"disk"`
	}
	if err := c.get("/1.0/instances/"+url.PathEscape(name)+"/state", &st); err != nil {
		return 0, err
	}
	return st.Disk["root"].Usage, nil
}

// streamError turns a non-2xx streaming response into an error, reading the
// Incus error envelope when there is one.
func (c *Client) streamError(op, name string, resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var env response
	if json.Unmarshal(raw, &env) == nil && env.Error != "" {
		return fmt.Errorf("incus %s %s: %s", op, name, env.Error)
	}
	return fmt.Errorf("incus %s %s: unexpected status %s", op, name, resp.Status)
}
