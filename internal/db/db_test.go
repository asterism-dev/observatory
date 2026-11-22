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

func TestNoLimitsTable(t *testing.T) {
	var limits bool
	if err := dbtest.New(t).QueryRow(context.Background(), "SELECT to_regclass('public.limits') IS NOT NULL").Scan(&limits); err != nil || limits {
		t.Fatalf("limits table %v %v, want none: quota owns limits", limits, err)
	}
}
