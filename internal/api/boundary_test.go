package api

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const storePkg = "github.com/asterism-dev/observatory/internal/store"

var iamReads = map[string]bool{"Membership": true, "Members": true, "Memberships": true, "Teams": true, "Team": true,
	"TeamMembers": true, "User": true, "Users": true, "UsersByID": true, "SSHKeysOf": true, "SessionByToken": true,
	"Sessions": true, "Invites": true, "Org": true, "Orgs": true}

// iamFiles lists per file the functions that may read IAM tables; nil allows the whole file.
var iamFiles = map[string][]string{}

func TestNoIAMReadsOutsideIAMFiles(t *testing.T) {
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "source", nil)
	for _, dir := range []string{".", "../fleet", "../network", "../images", "../builds", "../disks"} {
		files := parseDir(t, fset, dir)
		info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}}
		if _, err := (&types.Config{Importer: imp}).Check(dir, fset, files, info); err != nil {
			t.Fatalf("type-check %s: %v", dir, err)
		}
		for _, f := range files {
			allowed, listed := iamFiles[filepath.Base(fset.Position(f.Pos()).Filename)]
			if dir != "." {
				allowed, listed = nil, false
			}
			if listed && allowed == nil {
				continue
			}
			for _, d := range f.Decls {
				if fn, ok := d.(*ast.FuncDecl); ok && slices.Contains(allowed, fn.Name.Name) {
					continue
				}
				ast.Inspect(d, func(n ast.Node) bool {
					if sel, ok := n.(*ast.SelectorExpr); ok && isStoreIAMRead(info.Selections[sel]) {
						t.Errorf("%s: store.%s reads IAM data; ask the iam client or the policy", fset.Position(sel.Pos()), sel.Sel.Name)
					}
					return true
				})
			}
		}
	}
}

func parseDir(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

// isStoreIAMRead matches calls and method values alike: any selection of a denied method on *store.Store.
func isStoreIAMRead(s *types.Selection) bool {
	if s == nil || (s.Kind() != types.MethodVal && s.Kind() != types.MethodExpr) || !iamReads[s.Obj().Name()] {
		return false
	}
	recv := s.Obj().(*types.Func).Type().(*types.Signature).Recv()
	if recv == nil {
		return false
	}
	ptr, ok := recv.Type().(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := ptr.Elem().(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == storePkg && named.Obj().Name() == "Store"
}
