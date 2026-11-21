package authz

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var oldAuditActions = []string{
	"auth:login", "auth:cli-login", "auth:logout", "session:revoke", "org:create", "invite:accept", "invite:revoke",
	"member:leave", "team:member-set", "team:member-remove", "role:create", "role:update", "role:delete", "role:unbind",
	"sshkey:add", "sshkey:delete", "platform:grant-admin", "platform:revoke-admin", "platform:set-guest-mtu", "admin:read",
	"vm:snapshot-delete", "vm:delete-after", "disk:copy", "disk:grow", "disk:release", "node:enroll", "node:revoke",
	"env:move", "image:create", "image:update", "image:delete", "image:tag", "image:version-delete", "image:recipe",
	"image:edit", "image:build", "image:build-cancel", "registry:gc", "dashboard:delete", "dashboard:update",
	"telemetry_token:create", "telemetry_token:revoke", "vm:builders",
	"team:*", "env:*", "integration:*", "dashboard:*", "node:*", "vm:*", "disk:*",
}

func TestNoOldActionNames(t *testing.T) {
	var names []string
	for old := range renamed {
		names = append(names, regexp.QuoteMeta(old))
	}
	for _, old := range oldAuditActions {
		names = append(names, regexp.QuoteMeta(old))
	}
	re := regexp.MustCompile(`(?:^|[^A-Za-z0-9/._:-])(?:` + strings.Join(names, "|") + `)(?:$|[^A-Za-z0-9*-])`)

	root := filepath.Join("..", "..")
	skip := map[string]bool{"rename_test.go": true, "noold_test.go": true, "permission_test.go": true}
	check := func(path string) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if m := re.FindString(line); m != "" {
				t.Errorf("%s:%d: old action name %q", path, i+1, strings.Trim(m, " `'\"(),.;"))
			}
		}
	}
	for _, dir := range []string{"internal", "cmd", filepath.Join("web", "src")} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || skip[d.Name()] || strings.HasSuffix(path, ".png") || strings.HasSuffix(path, ".svg") {
				return err
			}
			check(path)
			return nil
		})
	}
	docs, _ := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	for _, p := range docs {
		check(p)
	}
}
