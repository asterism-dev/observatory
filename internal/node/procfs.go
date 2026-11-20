package node

import (
	"bufio"
	"errors"
	"io"
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

var cpuStates = [...]string{"user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal"}

func parseCPUTimes(r io.Reader) (map[string]map[string]float64, error) {
	out := map[string]map[string]float64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || !strings.HasPrefix(f[0], "cpu") || f[0] == "cpu" {
			continue
		}
		states := map[string]float64{}
		for i, name := range cpuStates {
			if i+1 >= len(f) {
				break
			}
			ticks, err := strconv.ParseInt(f[i+1], 10, 64)
			if err != nil {
				return nil, err
			}
			states[name] = float64(ticks) / clockTicks
		}
		out[f[0]] = states
	}
	return out, sc.Err()
}

func parseMemInfo(r io.Reader) (used, cached, free, swapUsed, swapFree int64, err error) {
	kb := map[string]int64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, perr := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		if perr == nil {
			kb[k] = n * 1024
		}
	}
	if err = sc.Err(); err != nil {
		return
	}
	if _, ok := kb["MemTotal"]; !ok {
		err = errors.New("meminfo: no MemTotal")
		return
	}
	free = kb["MemFree"]
	cached = kb["Cached"] + kb["SReclaimable"]
	used = kb["MemTotal"] - free - kb["Buffers"] - kb["Cached"] - kb["SReclaimable"]
	swapFree = kb["SwapFree"]
	swapUsed = kb["SwapTotal"] - swapFree
	return
}

func parseLoadAvg(r io.Reader) (l1, l5, l15 float64, err error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return
	}
	f := strings.Fields(string(raw))
	if len(f) < 3 {
		err = errors.New("loadavg: short line")
		return
	}
	if l1, err = strconv.ParseFloat(f[0], 64); err != nil {
		return
	}
	if l5, err = strconv.ParseFloat(f[1], 64); err != nil {
		return
	}
	l15, err = strconv.ParseFloat(f[2], 64)
	return
}

// Each tap carries VM traffic that the guests' own NICs would double count; the prefix mirrors linuxvm.TapName, which this package cannot import.
const vmTapPrefix = "vmm"

func parseNetDev(r io.Reader) (map[string][2]int64, error) {
	out := map[string][2]int64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "lo" || strings.HasPrefix(name, "tap") || strings.HasPrefix(name, vmTapPrefix) {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		rx, err1 := strconv.ParseInt(f[0], 10, 64)
		tx, err2 := strconv.ParseInt(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out[name] = [2]int64{rx, tx}
	}
	return out, sc.Err()
}
