package guestinit

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/version"
)

func TestMainDispatchesByArgv0(t *testing.T) {
	var got []string
	orig := handlers["vmm-token"]
	handlers["vmm-token"] = func(args []string) int { got = args; return 7 }
	t.Cleanup(func() { handlers["vmm-token"] = orig })

	if code := Main([]string{"/x/vmm-token", "a", "b"}); code != 7 {
		t.Fatalf("exit = %d", code)
	}
	if len(got) != 2 || got[0] != "a" {
		t.Fatalf("args = %v", got)
	}
}

func TestVersionFlag(t *testing.T) {
	var buf bytes.Buffer
	orig := stdout
	stdout = &buf
	t.Cleanup(func() { stdout = orig })

	if code := Main([]string{"observatory-init", "--version"}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if buf.String() != version.Version+"\n" {
		t.Fatalf("out = %q", buf.String())
	}
}

func TestToolVersionFlagReachesTool(t *testing.T) {
	var got []string
	orig := handlers["gh"]
	handlers["gh"] = func(args []string) int { got = args; return 0 }
	t.Cleanup(func() { handlers["gh"] = orig })

	Main([]string{"/.observatory/bin/gh", "--version"})
	if !slices.Equal(got, []string{"--version"}) {
		t.Fatalf("gh got %v, want [--version]", got)
	}
}

// switchedMountinfo is the init's mountinfo after the root switch: the image disk is "/".
const switchedMountinfo = `22 1 253:0 / / rw - ext4 /dev/vda rw
23 22 0:5 / /dev rw - devtmpfs devtmpfs rw
24 23 0:6 / /dev/pts rw - devpts devpts rw
25 22 253:16 / /data\040disk rw - ext4 /dev/vdc rw
26 22 253:1 / /data\040disk2 rw - ext4 /dev/vdd rw
27 22 0:3 / /proc rw - proc proc rw
28 22 0:7 / /run rw - tmpfs tmpfs rw
29 28 0:8 / /run/user/1 rw - tmpfs tmpfs rw
`

func TestMountsBelowDeepestFirst(t *testing.T) {
	for prefix, want := range map[string][]string{
		"/data disk": {"/data disk"},
		"/dev":       {"/dev/pts", "/dev"},
		"/":          {"/data disk2", "/run/user/1", "/data disk", "/dev/pts", "/proc", "/dev", "/run", "/"},
	} {
		if got := mountsBelow(strings.NewReader(switchedMountinfo), prefix); !slices.Equal(got, want) {
			t.Errorf("mountsBelow(%q) = %q, want %q", prefix, got, want)
		}
	}
}

func TestHasMount(t *testing.T) {
	for path, want := range map[string]bool{"/run": true, "/run/user": false, "/tmp": false} {
		if got := hasMount(strings.NewReader(switchedMountinfo), path); got != want {
			t.Errorf("hasMount(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestRunStepsRunsEveryStepDespiteFailures(t *testing.T) {
	var ran []string
	step := func(name string, err error) bootStep {
		return bootStep{name, func() error { ran = append(ran, name); return err }}
	}
	runSteps([]bootStep{step("a", errors.New("boom")), step("b", nil), step("c", errors.New("boom"))})
	if !slices.Equal(ran, []string{"a", "b", "c"}) {
		t.Fatalf("ran %v", ran)
	}
}
