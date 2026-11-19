package guestinit

import (
	"bytes"
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

func TestMountsBelowDeepestFirst(t *testing.T) {
	info := `22 1 253:0 / /newroot rw - ext4 /dev/vda rw
23 22 0:5 / /newroot/dev rw - devtmpfs devtmpfs rw
24 23 0:6 / /newroot/dev/pts rw - devpts devpts rw
25 22 253:16 / /newroot/data\040disk rw - ext4 /dev/vdc rw
26 1 253:1 / /newroot2 rw - ext4 /dev/vdd rw
27 1 0:3 / /proc rw - proc proc rw
`
	got := mountsBelow(strings.NewReader(info), "/newroot")
	want := []string{"/newroot/data disk", "/newroot/dev/pts", "/newroot/dev", "/newroot"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestHasMount(t *testing.T) {
	const info = "36 1 8:1 / /newroot rw - ext4 /dev/vda rw\n40 36 0:5 / /newroot/run rw - tmpfs tmpfs rw\n41 36 0:6 / /newroot/run/user/1 rw - tmpfs tmpfs rw\n"
	for path, want := range map[string]bool{"/newroot/run": true, "/newroot/run/user": false, "/newroot/dev": false} {
		if got := hasMount(strings.NewReader(info), path); got != want {
			t.Errorf("hasMount(%q) = %v, want %v", path, got, want)
		}
	}
}
