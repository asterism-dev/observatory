package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/platform/permission"
)

func TestAuditActionsValid(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "audit.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		vs, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if i >= len(vs.Values) || !strings.HasPrefix(name.Name, "Action") {
				continue
			}
			lit, ok := vs.Values[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			n++
			if v, _ := strconv.Unquote(lit.Value); !permission.ValidName(v) {
				t.Errorf("%s = %q is not service.resource.verb", name.Name, v)
			}
		}
		return true
	})
	if n < 50 {
		t.Fatalf("only %d action constants found", n)
	}
}
