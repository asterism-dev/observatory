package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrationsCheckClaudeNeedsOk(t *testing.T) {
	cases := map[string]struct {
		reply string
		want  string
	}{
		// "token" contains "ok": an auth failure must not pass.
		"auth failure": {"Failed to authenticate. API Error: 401 OAuth access token is invalid.", "FAIL claude -p"},
		"answer":       {"Ok", "PASS claude -p"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			bin := t.TempDir()
			fake := map[string]string{
				"ssh":       "#!/bin/sh\nshift\nexec \"$@\"\n",
				"claude":    "#!/bin/sh\n[ \"$1\" = -p ] && echo '" + c.reply + "'\nexit 0\n",
				"vmm-token": "#!/bin/sh\nexit 3\n",
				"timeout":   "#!/bin/sh\nshift\nexec \"$@\"\n",
				// Never the host's real gh, curl or sudo.
				"gh":   "#!/bin/sh\nexit 1\n",
				"curl": "#!/bin/sh\nexit 1\n",
				"sudo": "#!/bin/sh\nexit 1\n",
			}
			for n, body := range fake {
				if err := os.WriteFile(filepath.Join(bin, n), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("sh", "integrations-check.sh", "dummy@host")
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + t.TempDir(), "SSH=" + filepath.Join(bin, "ssh")}
			out, _ := cmd.CombinedOutput()
			if !strings.Contains(string(out), c.want+"\n") {
				t.Fatalf("want %q in:\n%s", c.want, out)
			}
		})
	}
}
