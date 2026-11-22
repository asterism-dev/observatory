package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/hubserver"
	"github.com/asterism-dev/observatory/internal/platform/permission"
)

// hubCheckDirs are the hub packages that check permissions, relative to this package.
var hubCheckDirs = []string{".", "../images", "../store"}

// TestHubAuthorizeActionsInCatalog keeps a hub check from naming an action the hub never registers, which would never grant.
func TestHubAuthorizeActionsInCatalog(t *testing.T) {
	cat, err := hubserver.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	names, pairs := map[string]bool{}, map[authz.Permission]bool{}
	for _, a := range cat.Actions {
		names[a.Name] = true
		pairs[authz.Permission{Action: a.Name, Object: a.Object}] = true
	}
	objConsts := authzObjectConsts(t)
	checked := 0
	for _, dir := range hubCheckDirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			checked += checkFile(t, name, objConsts, names, pairs)
		}
	}
	if checked < 50 {
		t.Errorf("found only %d literal actions; the walk is missing calls", checked)
	}
}

// checkFile checks every literal action passed to authorize, Allowed, allowed or a *FromPath helper; a literal or authz.Obj* object right before it must pair with it in the catalog.
func checkFile(t *testing.T, name string, objConsts map[string]string, names map[string]bool, pairs map[authz.Permission]bool) int {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
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
		if fn != "authorize" && fn != "Allowed" && fn != "allowed" && !strings.HasSuffix(fn, "FromPath") {
			return true
		}
		for i, arg := range call.Args {
			act, ok := stringLit(arg)
			if !ok || !permission.ValidName(act) {
				continue
			}
			checked++
			pos := fset.Position(arg.Pos())
			if !names[act] {
				t.Errorf("%s: %s is not in the hub catalog", pos, act)
				continue
			}
			if i == 0 {
				continue
			}
			obj, ok := stringLit(call.Args[i-1])
			if sel, isSel := call.Args[i-1].(*ast.SelectorExpr); isSel {
				if pkg, _ := sel.X.(*ast.Ident); pkg != nil && pkg.Name == "authz" {
					obj, ok = objConsts[sel.Sel.Name]
				}
			}
			if ok && !pairs[authz.Permission{Action: act, Object: obj}] {
				t.Errorf("%s: %s on %s is not in the hub catalog", pos, act, obj)
			}
		}
		return true
	})
	return checked
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// authzObjectConsts reads the Obj* string constants of internal/authz, so checks naming them can be paired with their object.
func authzObjectConsts(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "../authz/catalog.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 || !strings.HasPrefix(vs.Names[0].Name, "Obj") {
			return true
		}
		if s, ok := stringLit(vs.Values[0]); ok {
			out[vs.Names[0].Name] = s
		}
		return true
	})
	if len(out) < 20 {
		t.Fatalf("found only %d authz.Obj* constants", len(out))
	}
	return out
}
