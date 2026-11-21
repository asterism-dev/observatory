package db_test

import (
	"context"
	"testing"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
)

func TestHubHasOutboxInsteadOfAuditLog(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	var outbox, auditLog bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('public.outbox') IS NOT NULL, to_regclass('public.audit_log') IS NOT NULL").Scan(&outbox, &auditLog); err != nil {
		t.Fatal(err)
	}
	if !outbox || auditLog {
		t.Fatalf("outbox %v, audit_log %v", outbox, auditLog)
	}
}
