package authz

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

func repoFiles(t *testing.T) (all, web []string) {
	t.Helper()
	root := filepath.Join("..", "..")
	skip := map[string]bool{"rename_test.go": true, "noold_test.go": true, "permission_test.go": true}
	for _, dir := range []string{"internal", "cmd", filepath.Join("web", "src")} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || skip[d.Name()] || strings.HasSuffix(path, ".png") || strings.HasSuffix(path, ".svg") {
				return err
			}
			all = append(all, path)
			if strings.HasPrefix(path, filepath.Join(root, "web", "src")) {
				web = append(web, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	docs, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	return append(all, docs...), web
}

func TestNoOldActionNames(t *testing.T) {
	var names []string
	nouns := map[string]bool{}
	for old := range renamed {
		names = append(names, regexp.QuoteMeta(old))
		nouns[strings.SplitN(old, ":", 2)[0]] = true
	}
	for _, old := range oldAuditActions {
		names = append(names, regexp.QuoteMeta(old))
		nouns[strings.SplitN(old, ":", 2)[0]] = true
	}
	for n := range nouns {
		names = append(names, regexp.QuoteMeta(n)+`:\*`)
	}
	re := regexp.MustCompile(`(?:^|[^A-Za-z0-9/._:-])(?:` + strings.Join(names, "|") + `)(?:$|[^A-Za-z0-9*-])`)

	files, _ := repoFiles(t)
	if len(files) < 200 {
		t.Fatalf("scanned only %d files", len(files))
	}
	for _, path := range files {
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
}

func TestWebActionsExistInCatalog(t *testing.T) {
	known := map[string]bool{"*": true}
	for _, p := range LegacyCatalog {
		known[p.Action] = true
	}
	re := regexp.MustCompile(`(?:\bcan\(|\baction:\s*)'([^']+)'`)
	_, web := repoFiles(t)
	var found int
	for _, path := range web {
		if !slices.Contains([]string{".vue", ".ts"}, filepath.Ext(path)) || strings.HasSuffix(path, ".test.ts") {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			found++
			if !known[m[1]] {
				t.Errorf("%s: %q is not a catalog action", path, m[1])
			}
		}
	}
	if found < 30 {
		t.Fatalf("found only %d action strings in web/src", found)
	}
}
