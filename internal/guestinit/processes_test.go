package guestinit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

func writeProc(t *testing.T, proc string, pid int, cmdline, stat, uid string, rssPages int) {
	t.Helper()
	dir := filepath.Join(proc, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"cmdline": cmdline,
		"stat":    stat,
		"status":  "Name:\tx\nUid:\t" + uid + "\t" + uid + "\t" + uid + "\t" + uid + "\n",
		"statm":   fmt.Sprintf("1000 %d 10 1 0 5 0\n", rssPages),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// stat builds a /proc/<pid>/stat line; comm holds ") " to check parsing from the last paren.
func stat(pid, ppid int, state string, utime, stime int) string {
	return fmt.Sprintf("%d (we) ird) %s %d 1 1 0 -1 4194560 100 0 0 0 %d %d 0 0 20 0 1 0 50 1000 10\n", pid, state, ppid, utime, stime)
}

func procFixture(t *testing.T) (proc, passwd string) {
	t.Helper()
	proc, root := t.TempDir(), t.TempDir()
	passwd = filepath.Join(root, "passwd")
	if err := os.WriteFile(passwd, []byte("root:x:0:0:root:/root:/bin/sh\ndev:x:1000:1000::/home/dev:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("a", 600)
	writeProc(t, proc, 1, "/sbin/init\x00splash\x00", stat(1, 0, "S", 10, 10), "0", 100)
	writeProc(t, proc, 2, "", stat(2, 0, "I", 0, 0), "0", 0)
	writeProc(t, proc, 42, "node\x00"+long+"\x00", stat(42, 1, "R", 100, 0), "1001", 50)
	return proc, passwd
}

func TestProcessList(t *testing.T) {
	proc, passwd := procFixture(t)
	var slept time.Duration
	got, err := listProcesses(proc, passwd, func(d time.Duration) {
		slept = d
		// 20 ticks over 200 ms at USER_HZ 100 is 100 % of one core.
		writeProc(t, proc, 42, "node\x00"+strings.Repeat("a", 600)+"\x00", stat(42, 1, "R", 115, 5), "1001", 50)
	})
	if err != nil {
		t.Fatal(err)
	}
	if slept != 200*time.Millisecond {
		t.Fatalf("slept %s", slept)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	node, init := got[0], got[1]
	if node.PID != 42 || node.PPID != 1 || node.State != "R" || node.CPUPercent != 100 || node.User != "1001" || node.RSSBytes != 50*int64(os.Getpagesize()) {
		t.Fatalf("node = %+v", node)
	}
	if len(node.Command) != 512 || !strings.HasPrefix(node.Command, "node aaa") {
		t.Fatalf("command %d bytes: %q", len(node.Command), node.Command[:20])
	}
	if init.PID != 1 || init.User != "root" || init.Command != "/sbin/init splash" || init.CPUPercent != 0 || init.State != "S" {
		t.Fatalf("init = %+v", init)
	}
}

func TestProcessListSortsByRSSOnEqualCPU(t *testing.T) {
	proc, passwd := procFixture(t)
	got, err := listProcesses(proc, passwd, func(time.Duration) {})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].PID != 1 || got[1].PID != 42 {
		t.Fatalf("order = %d, %d", got[0].PID, got[1].PID)
	}
}

func TestProcessListCapped(t *testing.T) {
	proc, passwd := t.TempDir(), filepath.Join(t.TempDir(), "passwd")
	for pid := 1; pid <= 510; pid++ {
		writeProc(t, proc, pid, "x\x00", stat(pid, 0, "S", 0, 0), "0", pid)
	}
	got, err := listProcesses(proc, passwd, func(time.Duration) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 500 || got[0].PID != 510 || got[0].User != "0" {
		t.Fatalf("%d entries, first %+v", len(got), got[0])
	}
}

func TestAgentProcessesWhileBusy(t *testing.T) {
	a, _ := testAgent(t, webSeed)
	a.Proc, _ = procFixture(t)
	held := make(chan struct{})
	release := make(chan struct{})
	a.SetMACs = func([]string) error { close(held); <-release; return nil }
	tookOff := make(chan struct{})
	go func() { a.Handle([]byte(`{"op":"takeoff","entropy":"AAAAAAAAAAAAAAAAAAAAAA=="}`)); close(tookOff) }()
	<-held
	defer func() { close(release); <-tookOff }()
	done := make(chan []byte, 1)
	go func() { done <- a.Handle([]byte(`{"op":"processes"}`)) }()
	select {
	case b := <-done:
		var r struct {
			OK        bool
			Processes []proto.GuestProcess
		}
		if err := json.Unmarshal(b, &r); err != nil || !r.OK || len(r.Processes) != 2 {
			t.Fatalf("reply %s", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("processes waited for takeoff")
	}
}
