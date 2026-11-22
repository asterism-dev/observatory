package store_test

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/asterism-dev/observatory/internal/db"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/iam/store"
)

func TestIAMSchemaIsolated(t *testing.T) {
	ctx := context.Background()
	url := dbtest.Empty(t)
	if err := db.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	iamTables := []string{"users", "orgs", "memberships", "sessions", "invites", "teams", "team_members", "roles", "role_rules", "role_parents", "role_bindings", "ssh_keys", "secrets", "outbox"}
	tables := func(schema string) []string {
		t.Helper()
		rows, err := pool.Query(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema = $1", schema)
		if err != nil {
			t.Fatal(err)
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	inIAM := tables("iam")
	for _, name := range iamTables {
		if !slices.Contains(inIAM, name) {
			t.Errorf("iam.%s missing", name)
		}
	}
	for _, name := range tables("public") {
		if name != "outbox" && slices.Contains(iamTables, name) {
			t.Errorf("public.%s still exists after hub migrations", name)
		}
	}
	var leftover bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM public.hub_secrets WHERE name = 'iam-signing-key')").Scan(&leftover); err != nil || leftover {
		t.Errorf("hub still holds the iam signing key: %v %v", leftover, err)
	}
}

func TestSecretSealedAndStable(t *testing.T) {
	ctx := context.Background()
	pool := store.NewTestDB(t)
	s := store.New(pool)
	key := [32]byte{7}
	calls := 0
	gen := func() ([]byte, error) { calls++; return []byte("seed"), nil }
	first, err := s.Secret(ctx, key, "k", gen)
	if err != nil || string(first) != "seed" {
		t.Fatalf("first: %q %v", first, err)
	}
	if again, err := s.Secret(ctx, key, "k", gen); err != nil || string(again) != "seed" || calls != 1 {
		t.Fatalf("again: %q %v calls=%d", again, err, calls)
	}
	var sealed []byte
	if err := pool.QueryRow(ctx, "SELECT sealed FROM secrets WHERE name = 'k'").Scan(&sealed); err != nil || bytes.Contains(sealed, []byte("seed")) || len(sealed) < 12 {
		t.Fatalf("stored in the clear: %x %v", sealed, err)
	}
	if _, err := s.Secret(ctx, [32]byte{8}, "k", gen); err == nil {
		t.Fatal("opened with another key")
	}
}
