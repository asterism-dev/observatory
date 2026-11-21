package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/store/storetest"
)

func TestAuditWritesOutboxEvent(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	org := uuid.New()
	err := s.Audit(ctx, store.AuditEntry{ActorKind: store.ActorUser, ActorID: "a", OrgID: &org, Action: store.ActionMemberSetRole,
		Resource: "org/members/x", Result: store.ResultDenied, IP: "203.0.113.7", UserAgent: "ua", Details: map[string]any{"role": "admin", "id": org, "n": 2}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(ctx, store.AuditEntry{ActorKind: store.ActorSystem, Action: store.ActionAuthLogin, Result: store.ResultFailed}); err != nil {
		t.Fatal(err)
	}
	var subjects []string
	rows, err := pool.Query(ctx, "SELECT subject FROM outbox ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var subj string
		if err := rows.Scan(&subj); err != nil {
			t.Fatal(err)
		}
		subjects = append(subjects, subj)
	}
	if want := []string{"events." + org.String() + ".iam.members.setRole", "events.platform.iam.sessions.create"}; strings.Join(subjects, " ") != strings.Join(want, " ") {
		t.Fatalf("subjects %v, want %v", subjects, want)
	}
	es := storetest.AuditEvents(t, pool)
	if len(es) != 2 {
		t.Fatalf("%d events", len(es))
	}
	e := es[1]
	if !e.GetAudit() || e.GetType() != store.ActionMemberSetRole || e.GetOrg() != org.String() || e.GetActor().GetKind() != eventsv1.ActorKind_ACTOR_KIND_USER ||
		e.GetActor().GetId() != "a" || e.GetResource() != "org/members/x" || e.GetResult() != eventsv1.Result_RESULT_DENIED ||
		e.GetClientIp() != "203.0.113.7" || e.GetUserAgent() != "ua" || e.GetTime() == nil || e.GetId() == "" {
		t.Fatalf("event %v", e)
	}
	if d := e.GetDetails().AsMap(); d["role"] != "admin" || d["id"] != org.String() || d["n"] != 2.0 {
		t.Fatalf("details %v", d)
	}
	if sys := es[0]; sys.GetActor().GetKind() != eventsv1.ActorKind_ACTOR_KIND_SYSTEM || sys.GetOrg() != "" || sys.GetResult() != eventsv1.Result_RESULT_FAILED {
		t.Fatalf("system event %v", sys)
	}
}

func TestAuditInRolledBackTxDisappears(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
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
	if got := storetest.Audit(t, pool, ""); len(got) != 0 {
		t.Fatalf("got %v", got)
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
	pool := dbtest.New(t)
	s := store.New(pool)
	long := strings.Repeat("x", 2000)
	err := s.Audit(ctx, store.AuditEntry{
		ActorKind: store.ActorSystem, Action: store.ActionAuthLogin, Result: store.ResultFailed,
		UserAgent: "ua\x00" + long, Details: map[string]any{"reason": "r\x00" + long, "n": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := storetest.Audit(t, pool, "")
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	reason, _ := got[0].Details["reason"].(string)
	if got[0].UserAgent != ("ua" + long)[:512] || reason != ("r" + long)[:512] {
		t.Fatalf("not sanitized: ua %d bytes, reason %d bytes", len(got[0].UserAgent), len(reason))
	}
}

func TestAuditRejectsUnknownActorKindOrResult(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	for _, e := range []store.AuditEntry{
		{ActorKind: "", Action: store.ActionAuthLogin, Result: store.ResultAllowed},
		{ActorKind: "vm", Action: store.ActionAuthLogin, Result: store.ResultAllowed},
		{ActorKind: store.ActorSystem, Action: store.ActionAuthLogin, Result: ""},
		{ActorKind: store.ActorSystem, Action: store.ActionAuthLogin, Result: "maybe"},
	} {
		if err := s.Audit(ctx, e); err == nil {
			t.Errorf("%+v accepted", e)
		}
	}
	if got := storetest.Audit(t, pool, ""); len(got) != 0 {
		t.Fatalf("stored %v", got)
	}
}
