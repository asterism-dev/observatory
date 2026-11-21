package guestinit

import (
	"bytes"
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

const (
	processSample = 200 * time.Millisecond
	// /proc reports CPU times in USER_HZ, which Linux fixes at 100 for userspace.
	userHZ    = 100
	pfKthread = 0x200000
)

type procSample struct {
	proto.GuestProcess
	uid   string
	ticks uint64
}

// listProcesses reads proc twice, processSample apart, so CPU % covers that window.
func listProcesses(proc, passwd string, sleep func(time.Duration)) ([]proto.GuestProcess, error) {
	before, err := sampleProcesses(proc)
	if err != nil {
		return nil, err
	}
	sleep(processSample)
	after, err := sampleProcesses(proc)
	if err != nil {
		return nil, err
	}
	users := passwdNames(passwd)
	list := make([]proto.GuestProcess, 0, len(after))
	for pid, s := range after {
		if b, ok := before[pid]; ok && s.ticks >= b.ticks {
			s.CPUPercent = float64(s.ticks-b.ticks) / (processSample.Seconds() * userHZ) * 100
		}
		s.User = cmp.Or(users[s.uid], s.uid)
		list = append(list, s.GuestProcess)
	}
	slices.SortFunc(list, func(a, b proto.GuestProcess) int {
		return cmp.Or(cmp.Compare(b.CPUPercent, a.CPUPercent), cmp.Compare(b.RSSBytes, a.RSSBytes), cmp.Compare(a.PID, b.PID))
	})
	return list[:min(len(list), proto.MaxGuestProcesses)], nil
}

func sampleProcesses(proc string) (map[int]procSample, error) {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil, err
	}
	out := map[int]procSample{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if s, ok := readProcess(filepath.Join(proc, e.Name()), pid); ok {
			out[pid] = s
		}
	}
	return out, nil
}

// readProcess is false for kernel threads and processes that exited while being read.
func readProcess(dir string, pid int) (procSample, bool) {
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return procSample{}, false
	}
	// comm may contain spaces and parens, so the fields start after the last ')'.
	open, end := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')')
	if open < 0 || end < open {
		return procSample{}, false
	}
	f := strings.Fields(string(stat[end+1:]))
	if len(f) < 13 {
		return procSample{}, false
	}
	if flags, _ := strconv.ParseUint(f[6], 10, 64); flags&pfKthread != 0 {
		return procSample{}, false
	}
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return procSample{}, false
	}
	ppid, _ := strconv.Atoi(f[1])
	utime, _ := strconv.ParseUint(f[11], 10, 64)
	stime, _ := strconv.ParseUint(f[12], 10, 64)
	var rss int64
	if statm, err := os.ReadFile(filepath.Join(dir, "statm")); err == nil {
		if sf := strings.Fields(string(statm)); len(sf) > 1 {
			pages, _ := strconv.ParseInt(sf[1], 10, 64)
			rss = pages * int64(os.Getpagesize())
		}
	}
	cmd := strings.TrimRight(strings.ReplaceAll(string(cmdline), "\x00", " "), " ")
	if cmd == "" {
		// Zombies have no command line; ps shows their name in brackets too.
		cmd = "[" + string(stat[open+1:end]) + "]"
	}
	if len(cmd) > proto.MaxGuestCommand {
		cmd = strings.ToValidUTF8(cmd[:proto.MaxGuestCommand], "")
	}
	return procSample{
		GuestProcess: proto.GuestProcess{PID: pid, PPID: ppid, Command: cmd, State: f[0], RSSBytes: rss},
		uid:          effectiveUID(filepath.Join(dir, "status")),
		ticks:        utime + stime,
	}, true
}

func effectiveUID(status string) string {
	data, _ := os.ReadFile(status)
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			if f := strings.Fields(rest); len(f) > 1 {
				return f[1]
			}
		}
	}
	return ""
}

func passwdNames(path string) map[string]string {
	data, _ := os.ReadFile(path)
	names := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Split(line, ":"); len(f) >= 3 {
			names[f[2]] = f[0]
		}
	}
	return names
}
