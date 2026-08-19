package lx

import (
	"strings"
	"testing"
)

func TestParseMetrics(t *testing.T) {
	text := `# HELP ignored
# TYPE ignored gauge
incus_boot_time_seconds{name="alice",project="default",type="container"} 100
incus_cpu_seconds_total{cpu="0",mode="system",name="alice",project="default",type="container"} 1.25
incus_cpu_seconds_total{cpu="0",mode="user",name="alice",project="default",type="container"} 2.75
incus_memory_MemTotal_bytes{name="alice",project="default",type="container"} 2147483648
incus_memory_MemAvailable_bytes{name="alice",project="default",type="container"} 2000000000
incus_network_receive_bytes_total{device="eth0",name="alice",project="default",type="container"} 123
incus_network_transmit_bytes_total{device="eth0",name="alice",project="default",type="container"} 456
incus_filesystem_size_bytes{device="zfs",fstype="zfs",mountpoint="/",name="alice",project="default",type="container"} 1000
incus_filesystem_avail_bytes{device="zfs",fstype="zfs",mountpoint="/",name="alice",project="default",type="container"} 400
incus_procs_total{name="alice",project="default",type="container"} 13
incus_cpu_seconds_total{cpu="0",mode="user",name="ignored",project="default",type="virtual-machine"} 99
`
	got, err := parseMetrics(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	a := got["alice"]
	if a.BootTime != 100 || a.CPUSeconds != 4 || a.MemoryTotal != 2147483648 ||
		a.MemoryAvailable != 2000000000 || a.RxBytes != 123 || a.TxBytes != 456 ||
		a.FilesystemSize != 1000 || a.FilesystemAvail != 400 || a.Processes != 13 {
		t.Fatalf("unexpected aggregate: %+v", a)
	}
	if _, ok := got["ignored"]; ok {
		t.Fatal("virtual machine metric should not be included")
	}
}

func TestParseLabelsEscapes(t *testing.T) {
	name, labels, value, err := parseMetricLine(`metric{a="x\\y",b="line\nnext"} 1.5`)
	if err != nil {
		t.Fatal(err)
	}
	if name != "metric" || value != 1.5 || labels["a"] != "x\\y" || labels["b"] != "line\nnext" {
		t.Fatalf("unexpected parsed sample: %q %#v %v", name, labels, value)
	}
}
