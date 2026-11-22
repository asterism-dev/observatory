package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/asterism-dev/observatory/internal/db"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
)

func TestGuestImageMigrationRaisesUnprovisionedDisks(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	conn := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer conn.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, conn, db.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 14); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		WITH u AS (INSERT INTO users (subject, email, name) VALUES ('s', 'u@example.com', 'u') RETURNING id),
		o AS (INSERT INTO orgs (name) VALUES ('o') RETURNING id),
		n AS (INSERT INTO nodes (org_id, name) SELECT id, 'n' FROM o RETURNING id, org_id)
		INSERT INTO vms (org_id, node_id, user_id, name, cpus, memory_mb, disk_gb, provisioned, fork_snapshot_id)
		SELECT n.org_id, n.id, u.id, v.name, 1, 1024, v.disk_gb, v.provisioned, v.fork
		FROM n, u, (VALUES ('new', 5, false, NULL::uuid), ('big', 20, false, NULL), ('old', 5, true, NULL), ('fork', 5, false, gen_random_uuid()))
			AS v(name, disk_gb, provisioned, fork)`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 15); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"new": 10, "big": 20, "old": 5, "fork": 5}
	rows, err := pool.Query(ctx, "SELECT name, disk_gb FROM vms")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var gb int
		if err := rows.Scan(&name, &gb); err != nil {
			t.Fatal(err)
		}
		if gb != want[name] {
			t.Errorf("%s: disk_gb = %d, want %d", name, gb, want[name])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestMigration00017MarksExistingVMs(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	conn := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer conn.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, conn, db.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 16); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		WITH u AS (INSERT INTO users (subject, email, name) VALUES ('s', 'u@example.com', 'u') RETURNING id),
		o AS (INSERT INTO orgs (name) VALUES ('o') RETURNING id),
		n AS (INSERT INTO nodes (org_id, name) SELECT id, 'n' FROM o RETURNING id, org_id)
		INSERT INTO vms (org_id, node_id, user_id, name, cpus, memory_mb, disk_gb, provisioned)
		SELECT n.org_id, n.id, u.id, v.name, 1, 1024, 10, v.provisioned
		FROM n, u, (VALUES ('old', true), ('new', false)) AS v(name, provisioned)`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 17); err != nil {
		t.Fatal(err)
	}
	var old, fresh bool
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT legacy_runtime FROM vms WHERE name = 'old'), (SELECT legacy_runtime FROM vms WHERE name = 'new')`).Scan(&old, &fresh); err != nil {
		t.Fatal(err)
	}
	if !old || fresh {
		t.Fatalf("legacy_runtime old=%v new=%v, want true/false", old, fresh)
	}
}

func TestMigration00018RoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	conn := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer conn.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, conn, db.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		WITH u AS (SELECT gen_random_uuid() AS id),
		o AS (SELECT gen_random_uuid() AS id),
		i AS (INSERT INTO images (org_id, scope, name, created_by) SELECT o.id, 'org', 'img', u.id FROM o, u RETURNING id),
		r AS (INSERT INTO recipe_revisions (image_id, number, created_by) SELECT i.id, 1, u.id FROM i, u RETURNING image_id),
		f AS (INSERT INTO recipe_files (image_id, revision, path, mode, content) SELECT image_id, 1, 'Dockerfile', 420, '\x00' FROM r),
		b AS (INSERT INTO image_builds (image_id, revision, started_by) SELECT i.id, 1, u.id FROM i, u RETURNING id),
		v AS (INSERT INTO image_versions (image_id, digest, source) SELECT id, 'sha256:a', 'push' FROM i RETURNING image_id),
		t AS (INSERT INTO image_tags (image_id, tag, digest) SELECT image_id, 'latest', 'sha256:a' FROM v)
		INSERT INTO image_defaults (org_id, scope, image_id, tag) SELECT o.id, 'org', i.id, 'latest' FROM o, i`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 17); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := provider.UpTo(ctx, 18); err != nil {
		t.Fatalf("up: %v", err)
	}
}

func TestMigration00035MovesMgmtSliceToHubOrgs(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	conn := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer conn.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, conn, db.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 34); err != nil {
		t.Fatalf("down: %v", err)
	}
	var org, bare string
	if err := pool.QueryRow(ctx, "INSERT INTO orgs (name, mgmt_slice) VALUES ('o', '100.64.8.0/24') RETURNING id").Scan(&org); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO orgs (name) VALUES ('bare') RETURNING id").Scan(&bare); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 35); err != nil {
		t.Fatalf("up: %v", err)
	}
	var slice string
	if err := pool.QueryRow(ctx, "SELECT mgmt_slice::text FROM hub_orgs WHERE org_id = $1", org).Scan(&slice); err != nil || slice != "100.64.8.0/24" {
		t.Fatalf("hub_orgs: %q %v", slice, err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM hub_orgs").Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows: %d %v", n, err)
	}
	if _, err := provider.DownTo(ctx, 34); err != nil {
		t.Fatalf("down again: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT mgmt_slice::text FROM orgs WHERE id = $1", org).Scan(&slice); err != nil || slice != "100.64.8.0/24" {
		t.Fatalf("restored: %q %v", slice, err)
	}
}

func TestVMWithUnknownUserIDInserts(t *testing.T) {
	pool := dbtest.New(t)
	if _, err := pool.Exec(context.Background(), `
		WITH n AS (INSERT INTO nodes (org_id, name) VALUES (gen_random_uuid(), 'n') RETURNING id, org_id)
		INSERT INTO vms (org_id, node_id, user_id, created_by, name, cpus, memory_mb, disk_gb)
		SELECT n.org_id, n.id, gen_random_uuid(), gen_random_uuid(), 'v', 1, 1024, 10 FROM n`); err != nil {
		t.Fatal(err)
	}
}

func TestMigration00036BackfillsRegistryCredentials(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	conn := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer conn.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, conn, db.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 35); err != nil {
		t.Fatalf("down: %v", err)
	}
	var user string
	if err := pool.QueryRow(ctx, `
		WITH u AS (INSERT INTO users (subject, email, name) VALUES ('s', 'u@example.com', 'u') RETURNING id),
		s AS (
			INSERT INTO sessions (user_id, token_hash, kind, expires_at, revoked_at)
			SELECT u.id, v.hash, 'cli', v.expires, v.revoked FROM u,
				(VALUES ('\x01'::bytea, now() + interval '1 day', NULL::timestamptz), ('\x02', now() + interval '1 day', now()), ('\x03', now() - interval '1 hour', NULL))
				AS v(hash, expires, revoked)
			RETURNING id, token_hash)
		INSERT INTO registry_credentials (session_id, secret_hash, expires_at) SELECT id, token_hash, now() + interval '1 hour' FROM s
		RETURNING (SELECT id FROM u)`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 36); err != nil {
		t.Fatalf("up: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM registry_credentials").Scan(&n); err != nil || n != 1 {
		t.Fatalf("credentials after the backfill: %d %v, want only the live session's", n, err)
	}
	var hash []byte
	var got string
	var tail bool
	if err := pool.QueryRow(ctx, "SELECT secret_hash, user_id, session_expires_at > now() + interval '23 hours' FROM registry_credentials").Scan(&hash, &got, &tail); err != nil {
		t.Fatal(err)
	}
	if string(hash) != "\x01" || got != user || !tail {
		t.Fatalf("credential %x user %s (want %s) session expiry carried %v", hash, got, user, tail)
	}
	if _, err := provider.DownTo(ctx, 35); err != nil {
		t.Fatalf("down again: %v", err)
	}
}
