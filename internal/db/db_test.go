package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
)

func TestAuditLogIsAppendOnly(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)

	if _, err := pool.Exec(ctx, `INSERT INTO audit_log (actor_kind, actor_id, action, result) VALUES ('system','s','test','allowed')`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{`UPDATE audit_log SET action='x'`, `DELETE FROM audit_log`} {
		if _, err := pool.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s: want append-only error, got %v", stmt, err)
		}
	}
}
