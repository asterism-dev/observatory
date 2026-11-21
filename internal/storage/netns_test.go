package storage

import "testing"

func TestHasDefaultRoute(t *testing.T) {
	const header = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"
	live := header +
		"eth0\t00000000\t0100A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"wg-underlay\t0000C60A\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	orphaned := header + "wg-underlay\t0000C60A\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	for name, c := range map[string]struct {
		table string
		want  bool
	}{
		"live":     {live, true},
		"orphaned": {orphaned, false},
		"empty":    {header, false},
		"nothing":  {"", false},
	} {
		if got := HasDefaultRoute([]byte(c.table)); got != c.want {
			t.Errorf("%s: HasDefaultRoute = %v, want %v", name, got, c.want)
		}
	}
}
