package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/store"
)

func TestAuditFiltersAndPaging(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t))
	org := uuid.New()
	entries := []store.AuditEntry{
		{ActorKind: store.ActorUser, ActorID: "a", Action: store.ActionAuthLogin, Result: store.ResultAllowed},
		{ActorKind: store.ActorUser, ActorID: "a", Action: store.ActionOrgCreate, OrgID: &org, Result: store.ResultAllowed, Details: map[string]any{"k": "v"}},
		{ActorKind: store.ActorUser, ActorID: "b", Action: store.ActionAuthLogin, Result: store.ResultDenied},
		{ActorKind: store.ActorSystem, ActorID: "b", Action: store.ActionAuditRead, Result: store.ResultFailed},
		{ActorKind: store.ActorUser, ActorID: "a", Action: store.ActionAuthLogout, Result: store.ResultAllowed},
	}
	for _, e := range entries {
		if err := s.Audit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	count := func(f store.AuditFilter) int {
		t.Helper()
		got, err := s.AuditEntries(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	if n := count(store.AuditFilter{ActorID: "a"}); n != 3 {
		t.Fatalf("actor a: %d", n)
	}
	if n := count(store.AuditFilter{Action: store.ActionAuthLogin}); n != 2 {
		t.Fatalf("login: %d", n)
	}
	byOrg, _ := s.AuditEntries(ctx, store.AuditFilter{OrgID: &org})
	if len(byOrg) != 1 || byOrg[0].Details["k"] != "v" || byOrg[0].OrgID == nil || *byOrg[0].OrgID != org {
		t.Fatalf("org: %+v", byOrg)
	}

	var ids []int64
	var before int64
	for {
		page, err := s.AuditEntries(ctx, store.AuditFilter{Limit: 2, Before: before})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			ids = append(ids, e.ID)
		}
		before = page[len(page)-1].ID
	}
	if len(ids) != 5 {
		t.Fatalf("paged %d entries: %v", len(ids), ids)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] >= ids[i-1] {
			t.Fatalf("not newest-first without overlap: %v", ids)
		}
	}
}

func TestAuditInRolledBackTxDisappears(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t))
	boom := errors.New("boom")
	err := s.InTx(ctx, func(tx *store.Store) error {
		if err := tx.Audit(ctx, store.AuditEntry{ActorKind: store.ActorUser, ActorID: "a", Action: store.ActionAuthLogin, Result: store.ResultAllowed}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	got, err := s.AuditEntries(ctx, store.AuditFilter{})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestSanitizeText(t *testing.T) {
	for in, want := range map[string]string{
		"Mozilla/5.0":            "Mozilla/5.0",
		"a\x00b\nc\x7f":          "abc",
		"bad\xffutf8":            "badutf8",
		"Grüße":                  "Grüße",
		strings.Repeat("ü", 300): strings.Repeat("ü", 256),
	} {
		if got := store.SanitizeText(in); got != want {
			t.Errorf("SanitizeText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAuditSanitizesFreeText(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t))
	long := strings.Repeat("x", 2000)
	err := s.Audit(ctx, store.AuditEntry{
		ActorKind: store.ActorSystem, Action: store.ActionAuthLogin, Result: store.ResultFailed,
		UserAgent: "ua\x00" + long, Details: map[string]any{"reason": "r\x00" + long, "n": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.AuditEntries(ctx, store.AuditFilter{})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
	reason, _ := got[0].Details["reason"].(string)
	if got[0].UserAgent != ("ua" + long)[:512] || reason != ("r" + long)[:512] {
		t.Fatalf("not sanitized: ua %d bytes, reason %d bytes", len(got[0].UserAgent), len(reason))
	}
}
