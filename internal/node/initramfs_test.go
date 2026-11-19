package node

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

type cpioEntry struct {
	mode, rdevMajor, rdevMinor uint32
	data                       string
}

func readCpio(t *testing.T, b []byte) map[string]cpioEntry {
	t.Helper()
	out := map[string]cpioEntry{}
	align := func(n int) int { return (n + 3) &^ 3 }
	for off := 0; ; {
		if len(b) < off+110 || string(b[off:off+6]) != "070701" {
			t.Fatalf("bad header at %d", off)
		}
		field := func(i int) int {
			v, err := strconv.ParseUint(string(b[off+6+8*i:off+14+8*i]), 16, 32)
			if err != nil {
				t.Fatal(err)
			}
			return int(v)
		}
		size, nameSize := field(6), field(11)
		name := string(b[off+110 : off+110+nameSize-1])
		dataAt := align(off + 110 + nameSize)
		if name == "TRAILER!!!" {
			return out
		}
		out[name] = cpioEntry{mode: uint32(field(1)), rdevMajor: uint32(field(9)), rdevMinor: uint32(field(10)), data: string(b[dataAt : dataAt+size])}
		off = align(dataAt + size)
	}
}

func TestBuildInitramfsLayout(t *testing.T) {
	gb := t.TempDir()
	for _, n := range []string{"observatory-init", "mke2fs", "resize2fs", "vmlinux"} {
		if err := os.WriteFile(filepath.Join(gb, n), []byte(n+" bytes"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(t.TempDir(), "initramfs.cpio")
	if err := BuildInitramfs(gb, out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(out); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v", st.Mode())
	}
	got := readCpio(t, b)
	link := func(target string) cpioEntry { return cpioEntry{mode: syscall.S_IFLNK | 0o777, data: target} }
	exe := func(data string) cpioEntry { return cpioEntry{mode: syscall.S_IFREG | 0o755, data: data} }
	dir := cpioEntry{mode: syscall.S_IFDIR | 0o755}
	want := map[string]cpioEntry{
		"dev": dir, ".observatory": dir, ".observatory/bin": dir, ".observatory/libexec": dir,
		"dev/console":                       {mode: syscall.S_IFCHR | 0o600, rdevMajor: 5, rdevMinor: 1},
		"init":                              link(".observatory/bin/observatory-init"),
		".observatory/bin/observatory-init": exe("observatory-init bytes"),
		".observatory/libexec/mke2fs":       exe("mke2fs bytes"),
		".observatory/libexec/resize2fs":    exe("resize2fs bytes"),
	}
	for _, tool := range []string{"vmm-token", "git-credential-vmm", "gh", "agent-browser", "vmm-desktop"} {
		want[".observatory/bin/"+tool] = link("observatory-init")
	}
	if len(got) != len(want) {
		t.Errorf("%d entries, want %d: %v", len(got), len(want), got)
	}
	for name, w := range want {
		if g, ok := got[name]; !ok || g != w {
			t.Errorf("%s = %+v (present %v), want %+v", name, g, ok, w)
		}
	}
	if err := os.Remove(filepath.Join(gb, "resize2fs")); err != nil {
		t.Fatal(err)
	}
	if err := BuildInitramfs(gb, out); err == nil {
		t.Fatal("built without resize2fs")
	}
}
