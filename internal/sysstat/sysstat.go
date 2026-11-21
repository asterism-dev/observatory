package sysstat

import (
	"bufio"
	"errors"
	"io"
	"strconv"
	"strings"
)

const ClockTicks = 100

var cpuStates = [...]string{"user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal"}

type Mem struct{ Used, Cached, Free, SwapUsed, SwapFree int64 }

func CPUTimes(r io.Reader) (map[string]map[string]float64, error) {
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
			states[name] = float64(ticks) / ClockTicks
		}
		out[f[0]] = states
	}
	return out, sc.Err()
}

func MemInfo(r io.Reader) (Mem, error) {
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
	if err := sc.Err(); err != nil {
		return Mem{}, err
	}
	if _, ok := kb["MemTotal"]; !ok {
		return Mem{}, errors.New("meminfo: no MemTotal")
	}
	return Mem{
		Used:     kb["MemTotal"] - kb["MemFree"] - kb["Buffers"] - kb["Cached"] - kb["SReclaimable"],
		Cached:   kb["Cached"] + kb["SReclaimable"],
		Free:     kb["MemFree"],
		SwapUsed: kb["SwapTotal"] - kb["SwapFree"],
		SwapFree: kb["SwapFree"],
	}, nil
}

func LoadAvg(r io.Reader) (l1, l5, l15 float64, err error) {
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

// NetDev returns rx/tx bytes per device; lo is always skipped, skip may be nil.
func NetDev(r io.Reader, skip func(name string) bool) (map[string][2]int64, error) {
	out := map[string][2]int64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "lo" || (skip != nil && skip(name)) {
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
