package guest

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestContextFileSize(t *testing.T) {
	if n := len(ContextFile); n == 0 || n > 8192 {
		t.Fatalf("AGENTS.md is %d bytes; want 1..8192", n)
	}
}

func TestContextFileNamesExist(t *testing.T) {
	var all strings.Builder
	// The boot script installs through variables; spell its targets out as the guest sees them.
	all.WriteString(strings.NewReplacer(`$bin`, "/usr/local/bin", `$lib`, "/usr/local/lib/vmm", `$units`, "/etc/systemd/system", `$root`, "").Replace(BootScript))
	all.WriteString(AgentScript)
	all.WriteString(IntegrationsScript)
	for _, f := range []string{"../guestimage/manifest.json", "../guestimage/vmm-image-build.sh", "../guestimage/docker-daemon.json"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(b)
	}
	all.WriteString(DesktopScript)
	all.WriteString(BrowserScript)
	spans := regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(ContextFile, -1)
	if len(spans) == 0 {
		t.Fatal("no backticked names")
	}
	for _, m := range spans {
		name := strings.Fields(m[1])[0]
		if !strings.HasPrefix(name, "/") && !strings.HasPrefix(name, "vmm-") && name != "agent-browser" {
			continue
		}
		if strings.Contains(name, "<") {
			continue
		}
		if !strings.Contains(all.String(), name) {
			t.Errorf("AGENTS.md names %q, which no guest script or image file has", name)
		}
	}
}

func TestContextFileNoForbiddenAdvice(t *testing.T) {
	lower := strings.ToLower(ContextFile)
	for _, s := range []string{"boxd", "composio", "credential.helper store", "unset ssl_cert_file", "https_proxy="} {
		if strings.Contains(lower, s) {
			t.Errorf("AGENTS.md contains %q", s)
		}
	}
}
