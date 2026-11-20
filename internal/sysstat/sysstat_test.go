package sysstat

import (
	"strings"
	"testing"
)

const statFixture = `cpu  2255 34 2290 22625563 6290 127 456 0 0 0
cpu0 1234 34 1145 11312781 3145 60 228 5 0 0
cpu1 1021 0 1145 11312782 3145 67 228 0 0 0
intr 114930548 113199788 3 0 5 263 0 4 [... lots more numbers ...]
ctxt 1990473
`

func TestCPUTimes(t *testing.T) {
	got, err := CPUTimes(strings.NewReader(statFixture))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["cpu"]; ok || len(got) != 2 {
		t.Fatalf("aggregate line must be ignored: %v", got)
	}
	if got["cpu0"]["user"] != 12.34 || got["cpu0"]["steal"] != 0.05 || got["cpu1"]["idle"] != 113127.82 {
		t.Fatalf("got %v", got)
	}
}

const meminfoFixture = `MemTotal:       16384000 kB
MemFree:         2048000 kB
MemAvailable:    9000000 kB
Buffers:          512000 kB
Cached:          4096000 kB
SwapCached:            0 kB
SwapTotal:       2097152 kB
SwapFree:        1048576 kB
SReclaimable:     256000 kB
`

func TestMemInfo(t *testing.T) {
	got, err := MemInfo(strings.NewReader(meminfoFixture))
	if err != nil {
		t.Fatal(err)
	}
	kb := int64(1024)
	want := Mem{Used: (16384000 - 2048000 - 512000 - 4096000 - 256000) * kb, Cached: (4096000 + 256000) * kb, Free: 2048000 * kb,
		SwapUsed: 1048576 * kb, SwapFree: 1048576 * kb}
	if got != want {
		t.Fatalf("got %+v", got)
	}
	if _, err := MemInfo(strings.NewReader("")); err == nil {
		t.Fatal("empty meminfo must fail")
	}
}

func TestLoadAvg(t *testing.T) {
	l1, l5, l15, err := LoadAvg(strings.NewReader("0.52 0.58 0.59 2/467 12345\n"))
	if err != nil || l1 != 0.52 || l5 != 0.58 || l15 != 0.59 {
		t.Fatalf("got %v %v %v %v", l1, l5, l15, err)
	}
}

const netDevFixture = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1000 10 0 0 0 0 0 0 1000 10 0 0 0 0 0 0
  eth0: 123456 100 0 0 0 0 0 0 654321 90 0 0 0 0 0 0
vmm1a2b3c4d0: 777 7 0 0 0 0 0 0 888 8 0 0 0 0 0 0
vmmdeadbeef12: 777 7 0 0 0 0 0 0 888 8 0 0 0 0 0 0
  vmm0: 300 3 0 0 0 0 0 0 400 4 0 0 0 0 0 0
tap0: 5 1 0 0 0 0 0 0 6 1 0 0 0 0 0 0
ovs-system: 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0
br-int: 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0
genev_sys_6081: 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0
vxlan_sys_4789: 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0
gre_sys: 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0
wg-underlay: 10 1 0 0 0 0 0 0 20 2 0 0 0 0 0 0
`

func TestNetDevSkip(t *testing.T) {
	got, err := NetDev(strings.NewReader(netDevFixture), func(name string) bool { return strings.HasPrefix(name, "vmm") && name != "vmm0" })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 9 || got["eth0"] != [2]int64{123456, 654321} || got["vmm0"] != [2]int64{300, 400} || got["tap0"] != [2]int64{5, 6} {
		t.Fatalf("got %v", got)
	}
	if _, ok := got["lo"]; ok {
		t.Fatal("lo must always be skipped")
	}
	if _, ok := got["vmmdeadbeef12"]; ok {
		t.Fatal("skip func ignored")
	}
}
