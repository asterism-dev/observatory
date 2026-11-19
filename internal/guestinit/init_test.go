package guestinit

import (
	"bytes"
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
