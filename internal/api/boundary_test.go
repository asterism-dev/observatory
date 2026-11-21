package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var iamReads = map[string]bool{"Membership": true, "Members": true, "Memberships": true, "Teams": true, "Team": true,
	"TeamMembers": true, "User": true, "Users": true, "SSHKeysOf": true, "SessionByToken": true}

// iamFiles may read IAM tables; for api.go only the listed functions may.
var iamFiles = map[string][]string{"members.go": nil, "teams.go": nil, "roles.go": nil, "orgs.go": nil, "admin.go": nil, "sshkeys.go": nil, "api.go": {"me", "sessions"}}

func TestNoIAMReadsOutsideIAMFiles(t *testing.T) {
	for _, dir := range []string{".", "../fleet", "../network", "../images"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			allowed, listed := iamFiles[filepath.Base(path)]
			if dir != "." {
				allowed, listed = nil, false
			}
			if listed && allowed == nil {
				continue
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range f.Decls {
				if fn, ok := d.(*ast.FuncDecl); ok && slices.Contains(allowed, fn.Name.Name) {
					continue
				}
				ast.Inspect(d, func(n ast.Node) bool {
					if name, ok := iamRead(n); ok {
						t.Errorf("%s: %s reads IAM data; ask the iam client or the policy", fset.Position(n.Pos()), name)
					}
					return true
				})
			}
		}
	}
}

// iamRead matches x.Name(ctx, …) for a store-style IAM read; the enforcer's TeamMembers(org, team) takes no context.
func iamRead(n ast.Node) (string, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !iamReads[sel.Sel.Name] {
		return "", false
	}
	switch a := call.Args[0].(type) {
	case *ast.Ident:
		return sel.Sel.Name, a.Name == "ctx"
	case *ast.CallExpr:
		s, ok := a.Fun.(*ast.SelectorExpr)
		return sel.Sel.Name, ok && (s.Sel.Name == "Context" || s.Sel.Name == "Background" || s.Sel.Name == "TODO")
	}
	return "", false
}
