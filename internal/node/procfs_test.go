package node

import (
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/sysstat"
)

func TestParseOSRelease(t *testing.T) {
	in := "NAME=\"Debian GNU/Linux\"\nPRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nID=debian\n"
	if got := parseOSRelease(strings.NewReader(in)); got != "Debian GNU/Linux 13 (trixie)" {
		t.Fatalf("got %q", got)
	}
}

func TestParseCPUModel(t *testing.T) {
	in := "processor\t: 0\nvendor_id\t: AuthenticAMD\nmodel name\t: AMD EPYC 7302\nprocessor\t: 1\nmodel name\t: AMD EPYC 7302\n"
	if got := parseCPUModel(strings.NewReader(in)); got != "AMD EPYC 7302" {
		t.Fatalf("got %q", got)
	}
}

func TestParseMemTotalMB(t *testing.T) {
	in := "MemTotal:       16384000 kB\nMemFree:         1000 kB\n"
	if got := parseMemTotalMB(strings.NewReader(in)); got != 16000 {
		t.Fatalf("got %d", got)
	}
}

const statFixture = `cpu  2255 34 2290 22625563 6290 127 456 0 0 0
cpu0 1234 34 1145 11312781 3145 60 228 5 0 0
cpu1 1021 0 1145 11312782 3145 67 228 0 0 0
intr 114930548 113199788 3 0 5 263 0 4 [... lots more numbers ...]
ctxt 1990473
`

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

func TestSkipNetDevice(t *testing.T) {
	got, err := sysstat.NetDev(strings.NewReader(netDevFixture), skipNetDevice)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["wg-underlay"] != [2]int64{10, 20} || got["eth0"] != [2]int64{123456, 654321} || got["vmm0"] != [2]int64{300, 400} {
		t.Fatalf("got %v", got)
	}
}
