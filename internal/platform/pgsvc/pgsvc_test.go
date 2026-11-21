package pgsvc_test

import (
	"context"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/platform/pgsvc"
)

func migrations(table string) fstest.MapFS {
	return fstest.MapFS{
		"00001_init.sql": {Data: []byte("-- +goose Up\nCREATE TABLE " + table + " (id int);\n-- +goose Down\nDROP TABLE " + table + ";\n")},
	}
}

func TestSchemasMigrateIndependently(t *testing.T) {
	ctx := context.Background()
	url := dbtest.Empty(t)

	a, err := pgsvc.Open(ctx, url, "svc_a", migrations("things"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := pgsvc.Open(ctx, url, "svc_b", migrations("other"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	var n int
	for _, schema := range []string{"svc_a", "svc_b"} {
		if err := a.QueryRow(ctx, "SELECT count(*) FROM "+schema+".goose_db_version").Scan(&n); err != nil {
			t.Fatalf("%s version table: %v", schema, err)
		}
		if n != 2 {
			t.Fatalf("%s version rows = %d, want 2", schema, n)
		}
	}
}

func TestSearchPathIsolation(t *testing.T) {
	ctx := context.Background()
	url := dbtest.Empty(t)
	a, err := pgsvc.Open(ctx, url, "svc_a", migrations("things"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := pgsvc.Open(ctx, url, "svc_b", migrations("other"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if _, err := a.Exec(ctx, "SELECT * FROM things"); err != nil {
		t.Fatalf("A cannot see its own table: %v", err)
	}
	if _, err := b.Exec(ctx, "SELECT * FROM things"); err == nil {
		t.Fatal("B sees A's table unqualified")
	}
}

func TestInvalidSchemaRejected(t *testing.T) {
	for _, schema := range []string{"", "Audit", "1abc", "a-b", "a;drop", "public.x", "a\"b", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		if _, err := pgsvc.Open(context.Background(), "postgres://unused", schema, migrations("t")); err == nil {
			t.Errorf("schema %q accepted", schema)
		}
	}
}

func TestReopenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	url := dbtest.Empty(t)
	for range 2 {
		p, err := pgsvc.Open(ctx, url, "svc_a", migrations("things"))
		if err != nil {
			t.Fatal(err)
		}
		p.Close()
	}
}

func TestConcurrentOpensMigrateOnce(t *testing.T) {
	ctx := context.Background()
	url := dbtest.Empty(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := pgsvc.Open(ctx, url, "svc_a", migrations("things"))
			if err == nil {
				p.Close()
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
