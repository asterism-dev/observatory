package node

import (
	"bufio"
	"io"
	"regexp"
	"strconv"
	"strings"
)

func parseOSRelease(r io.Reader) string {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

func parseCPUModel(r io.Reader) string {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func parseMemTotalMB(r io.Reader) int {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			kb, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(v), " kB"))
			return kb / 1024
		}
	}
	return 0
}

// Each tap carries VM traffic that the guests' own NICs would double count; the shape mirrors linuxvm.TapName, which this package cannot import.
var vmTapName = regexp.MustCompile(`^vmm[0-9a-f]{8}[0-9]+$`)

// OVS internal devices carry no traffic of their own; it shows up on the physical and wg interfaces.
var ovsDevice = regexp.MustCompile(`^(ovs-system|br-int|gre_sys|genev_sys_.*|vxlan_sys_.*)$`)

func skipNetDevice(name string) bool {
	return strings.HasPrefix(name, "tap") || vmTapName.MatchString(name) || ovsDevice.MatchString(name)
}
