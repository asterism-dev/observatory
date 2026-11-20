package node

import (
	"strings"
	"testing"
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

func TestParseCPUTimes(t *testing.T) {
	got, err := parseCPUTimes(strings.NewReader(statFixture))
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

func TestParseMemInfo(t *testing.T) {
	used, cached, free, swapUsed, swapFree, err := parseMemInfo(strings.NewReader(meminfoFixture))
	if err != nil {
		t.Fatal(err)
	}
	kb := int64(1024)
	if used != (16384000-2048000-512000-4096000-256000)*kb || cached != (4096000+256000)*kb || free != 2048000*kb ||
		swapUsed != 1048576*kb || swapFree != 1048576*kb {
		t.Fatalf("got %d %d %d %d %d", used, cached, free, swapUsed, swapFree)
	}
	if _, _, _, _, _, err := parseMemInfo(strings.NewReader("")); err == nil {
		t.Fatal("empty meminfo must fail")
	}
}

func TestParseLoadAvg(t *testing.T) {
	l1, l5, l15, err := parseLoadAvg(strings.NewReader("0.52 0.58 0.59 2/467 12345\n"))
	if err != nil || l1 != 0.52 || l5 != 0.58 || l15 != 0.59 {
		t.Fatalf("got %v %v %v %v", l1, l5, l15, err)
	}
}

const netDevFixture = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1000 10 0 0 0 0 0 0 1000 10 0 0 0 0 0 0
  eth0: 123456 100 0 0 0 0 0 0 654321 90 0 0 0 0 0 0
vmm1a2b3c4d0: 777 7 0 0 0 0 0 0 888 8 0 0 0 0 0 0
tap0: 5 1 0 0 0 0 0 0 6 1 0 0 0 0 0 0
`

func TestParseNetDevSkipsLoAndTaps(t *testing.T) {
	got, err := parseNetDev(strings.NewReader(netDevFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["eth0"] != [2]int64{123456, 654321} {
		t.Fatalf("got %v", got)
	}
}
