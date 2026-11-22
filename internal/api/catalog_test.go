package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/hubserver"
	"github.com/asterism-dev/observatory/internal/platform/permission"
)

// TestHubAuthorizeActionsInCatalog keeps a handler from checking an action the hub never registers, which would never grant.
func TestHubAuthorizeActionsInCatalog(t *testing.T) {
	known := map[string]bool{}
	for _, a := range hubserver.Catalog().Actions {
		known[a.Name] = true
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var fn string
			switch c := call.Fun.(type) {
			case *ast.SelectorExpr:
				fn = c.Sel.Name
			case *ast.Ident:
				fn = c.Name
			}
			if fn != "authorize" && fn != "Allowed" {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				act, err := strconv.Unquote(lit.Value)
				if err != nil || !permission.ValidName(act) {
					continue
				}
				checked++
				if !known[act] {
					t.Errorf("%s: %s is not in the hub catalog", fset.Position(lit.Pos()), act)
				}
			}
			return true
		})
	}
	if checked < 50 {
		t.Errorf("found only %d literal actions; the walk is missing calls", checked)
	}
}
