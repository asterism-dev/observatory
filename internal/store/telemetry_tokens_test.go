package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/store"
)

func TestTelemetryTokens(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	a, b := login(t, s, "a"), login(t, s, "b")
	orgA := mustCreateOrg(t, s, "A", a.User.ID)
	orgB := mustCreateOrg(t, s, "B", b.User.ID)

	tok, secret, err := s.CreateTelemetryToken(ctx, orgA.ID, a.User.ID, "grafana", 0)
	if err != nil || !strings.HasPrefix(secret, "obs_grafana_") || len(secret) != len("obs_grafana_")+52 {
		t.Fatalf("create %q %v", secret, err)
	}
	if tok.Name != "grafana" || tok.CreatedBy != a.User.ID || tok.LastUsedAt != nil || tok.RevokedAt != nil {
		t.Fatalf("token %+v", tok)
	}

	var stored int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM telemetry_tokens t WHERE t::text LIKE '%' || $1 || '%'", secret).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("plaintext stored: %d %v", stored, err)
	}

	got, err := s.TelemetryTokenBySecret(ctx, secret)
	if err != nil || got.ID != tok.ID || got.OrgID != orgA.ID {
		t.Fatalf("by secret %+v %v", got, err)
	}
	if _, err := s.TelemetryTokenBySecret(ctx, secret+"x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("wrong secret: %v", err)
	}

	if _, _, err := s.CreateTelemetryToken(ctx, orgA.ID, a.User.ID, "grafana", 0); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, _, err := s.CreateTelemetryToken(ctx, orgB.ID, b.User.ID, "grafana", 0); err != nil {
		t.Fatalf("same name in other org: %v", err)
	}
	list, err := s.TelemetryTokens(ctx, orgA.ID)
	if err != nil || len(list) != 1 || list[0].ID != tok.ID {
		t.Fatalf("list %+v %v", list, err)
	}

	if err := s.TouchTelemetryToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	first, _ := s.TelemetryTokenBySecret(ctx, secret)
	if first.LastUsedAt == nil {
		t.Fatal("not touched")
	}
	if err := s.TouchTelemetryToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.TelemetryTokenBySecret(ctx, secret); !again.LastUsedAt.Equal(*first.LastUsedAt) {
		t.Fatal("touch not throttled")
	}
	execSQL(t, pool, "UPDATE telemetry_tokens SET last_used_at = now() - interval '2 minutes' WHERE id = $1", tok.ID)
	if err := s.TouchTelemetryToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if later, _ := s.TelemetryTokenBySecret(ctx, secret); !later.LastUsedAt.After(*first.LastUsedAt) {
		t.Fatal("stale touch not refreshed")
	}

	if _, err := s.RevokeTelemetryToken(ctx, orgB.ID, tok.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign revoke: %v", err)
	}
	if name, err := s.RevokeTelemetryToken(ctx, orgA.ID, tok.ID); err != nil || name != tok.Name {
		t.Fatal(name, err)
	}
	if _, err := s.TelemetryTokenBySecret(ctx, secret); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked still resolves: %v", err)
	}
	if list, _ := s.TelemetryTokens(ctx, orgA.ID); len(list) != 0 {
		t.Fatalf("revoked listed: %+v", list)
	}
	if _, err := s.RevokeTelemetryToken(ctx, orgA.ID, tok.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("double revoke: %v", err)
	}
	if _, _, err := s.CreateTelemetryToken(ctx, orgA.ID, a.User.ID, "grafana", 0); err != nil {
		t.Fatalf("name reusable after revoke: %v", err)
	}
}

func TestTelemetryTokenLimit(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t))
	a := login(t, s, "a")
	org := mustCreateOrg(t, s, "A", a.User.ID)
	var first store.TelemetryToken
	for i := 0; i < 20; i++ {
		tok, _, err := s.CreateTelemetryToken(ctx, org.ID, a.User.ID, fmt.Sprintf("t%d", i), 0)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = tok
		}
	}
	if _, _, err := s.CreateTelemetryToken(ctx, org.ID, a.User.ID, "extra", 0); !errors.Is(err, store.ErrLimitExceeded) {
		t.Fatalf("21st: %v", err)
	}
	if _, err := s.RevokeTelemetryToken(ctx, org.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateTelemetryToken(ctx, org.ID, a.User.ID, "extra", 0); err != nil {
		t.Fatalf("after revoke: %v", err)
	}
}
