package lx

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Metric is one sample from Incus's Prometheus exposition endpoint.
type Metric struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// InstanceMetrics is the subset of metrics used by the resource sampler. The
// byte counters are cumulative Incus counters; the manager turns them into
// per-sample deltas before storing historical data.
type InstanceMetrics struct {
	BootTime        int64
	CPUSeconds      float64
	EffectiveCPUs   float64
	MemoryTotal     int64
	MemoryAvailable int64
	Processes       int64
	RxBytes         int64
	TxBytes         int64
	DiskReadBytes   int64
	DiskWriteBytes  int64
	FilesystemSize  int64
	FilesystemAvail int64
}

// Metrics returns the current metrics from Incus over the configured Unix
// socket. Unlike normal Incus API calls, /1.0/metrics returns raw Prometheus
// text rather than an Incus JSON response envelope.
func (c *Client) Metrics() (map[string]InstanceMetrics, error) {
	req, err := http.NewRequest(http.MethodGet, c.base+"/1.0/metrics", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("incus metrics: status %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return parseMetrics(resp.Body)
}

func parseMetrics(r io.Reader) (map[string]InstanceMetrics, error) {
	type sample struct {
		name   string
		value  float64
		labels map[string]string
	}
	var samples []sample
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, err := parseMetricLine(line)
		if err != nil {
			return nil, err
		}
		samples = append(samples, sample{name: name, labels: labels, value: value})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	out := make(map[string]InstanceMetrics)
	for _, s := range samples {
		name := s.labels["name"]
		if name == "" || s.labels["type"] != "container" {
			continue
		}
		v := out[name]
		switch s.name {
		case "incus_boot_time_seconds":
			v.BootTime = int64(s.value)
		case "incus_cpu_seconds_total":
			v.CPUSeconds += s.value
		case "incus_cpu_effective_total":
			v.EffectiveCPUs = s.value
		case "incus_memory_MemTotal_bytes":
			v.MemoryTotal = addMetricInt(v.MemoryTotal, s.value)
		case "incus_memory_MemAvailable_bytes":
			v.MemoryAvailable = addMetricInt(v.MemoryAvailable, s.value)
		case "incus_procs_total":
			v.Processes = addMetricInt(v.Processes, s.value)
		case "incus_network_receive_bytes_total":
			v.RxBytes = addMetricInt(v.RxBytes, s.value)
		case "incus_network_transmit_bytes_total":
			v.TxBytes = addMetricInt(v.TxBytes, s.value)
		case "incus_disk_read_bytes_total":
			v.DiskReadBytes = addMetricInt(v.DiskReadBytes, s.value)
		case "incus_disk_written_bytes_total":
			v.DiskWriteBytes = addMetricInt(v.DiskWriteBytes, s.value)
		case "incus_filesystem_size_bytes":
			if s.labels["mountpoint"] == "/" {
				v.FilesystemSize = addMetricInt(v.FilesystemSize, s.value)
			}
		case "incus_filesystem_avail_bytes":
			if s.labels["mountpoint"] == "/" {
				v.FilesystemAvail = addMetricInt(v.FilesystemAvail, s.value)
			}
		}
		out[name] = v
	}

	for name, v := range out {
		if v.FilesystemSize > 0 && v.FilesystemAvail <= v.FilesystemSize {
			// Incus exposes available space, which is the useful value for a
			// container user. Store used space in the manager layer.
			out[name] = v
		}
	}
	return out, nil
}

func addMetricInt(current int64, value float64) int64 {
	if value <= 0 {
		return current
	}
	return current + int64(value)
}

func parseMetricLine(line string) (string, map[string]string, float64, error) {
	space := strings.LastIndexByte(line, ' ')
	if space < 1 {
		return "", nil, 0, fmt.Errorf("incus metrics: malformed sample %q", line)
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(line[space+1:]), 64)
	if err != nil {
		return "", nil, 0, fmt.Errorf("incus metrics: invalid value in %q: %w", line, err)
	}
	header := line[:space]
	labels := map[string]string{}
	name := header
	if open := strings.IndexByte(header, '{'); open >= 0 {
		if !strings.HasSuffix(header, "}") || open == 0 {
			return "", nil, 0, fmt.Errorf("incus metrics: malformed labels in %q", line)
		}
		name = header[:open]
		var err error
		labels, err = parseLabels(header[open+1 : len(header)-1])
		if err != nil {
			return "", nil, 0, fmt.Errorf("incus metrics: %w", err)
		}
	}
	return name, labels, value, nil
}

func parseLabels(s string) (map[string]string, error) {
	labels := map[string]string{}
	for len(strings.TrimSpace(s)) > 0 {
		s = strings.TrimSpace(s)
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("malformed label %q", s)
		}
		key := strings.TrimSpace(s[:eq])
		s = strings.TrimSpace(s[eq+1:])
		if len(s) == 0 || s[0] != '"' {
			return nil, fmt.Errorf("malformed label value for %q", key)
		}
		var b strings.Builder
		escaped := false
		end := -1
		for i := 1; i < len(s); i++ {
			if escaped {
				switch s[i] {
				case 'n':
					b.WriteByte('\n')
				default:
					b.WriteByte(s[i])
				}
				escaped = false
				continue
			}
			if s[i] == '\\' {
				escaped = true
				continue
			}
			if s[i] == '"' {
				end = i
				break
			}
			b.WriteByte(s[i])
		}
		if end < 0 || escaped {
			return nil, fmt.Errorf("unterminated label %q", key)
		}
		labels[key] = b.String()
		s = strings.TrimSpace(s[end+1:])
		if s == "" {
			break
		}
		if s[0] != ',' {
			return nil, fmt.Errorf("expected comma after label %q", key)
		}
		s = s[1:]
	}
	return labels, nil
}
