package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/iam/store"
)

const week = 7 * 24 * time.Hour

func TestInviteLifecycle(t *testing.T) {
	ctx := context.Background()
	s := store.New(store.NewTestDB(t))
	a, b, c := login(t, s, "a"), login(t, s, "b"), login(t, s, "c")
	org, err := s.CreateOrg(ctx, "Acme", a.User.ID)
	if err != nil {
		t.Fatal(err)
	}

	token, inv, err := s.CreateInvite(ctx, org.ID, store.RoleAdmin, a.User.ID, week)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "obsinv_") || len(token) != len("obsinv_")+43 {
		t.Fatalf("token %q", token)
	}
	if inv.OrgID != org.ID || inv.Role != store.RoleAdmin || inv.CreatedBy != a.User.ID || inv.ExpiresAt.Sub(inv.CreatedAt) != week {
		t.Fatalf("invite: %+v", inv)
	}
	if list, err := s.Invites(ctx, org.ID); err != nil || len(list) != 1 || list[0].ID != inv.ID {
		t.Fatalf("invites: %+v %v", list, err)
	}

	// The org owner can't use an invite; it stays valid.
	if _, _, err := s.AcceptInvite(ctx, token, a.User.ID); !errors.Is(err, store.ErrAlreadyMember) {
		t.Fatalf("accept by member: %v", err)
	}
	got, used, err := s.AcceptInvite(ctx, token, b.User.ID)
	if err != nil || got.ID != org.ID || got.Name != "Acme" || used.ID != inv.ID || used.Role != store.RoleAdmin || used.CreatedBy != a.User.ID {
		t.Fatalf("accept: %+v %+v %v", got, used, err)
	}
	if m, err := s.Membership(ctx, org.ID, b.User.ID); err != nil || m.Role != store.RoleAdmin {
		t.Fatalf("membership: %+v %v", m, err)
	}
	if _, _, err := s.AcceptInvite(ctx, token, c.User.ID); !errors.Is(err, store.ErrInviteInvalid) {
		t.Fatalf("reuse: %v", err)
	}
	if list, _ := s.Invites(ctx, org.ID); len(list) != 0 {
		t.Fatalf("used invite listed: %+v", list)
	}

	expired, _, err := s.CreateInvite(ctx, org.ID, store.RoleMember, a.User.ID, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptInvite(ctx, expired, c.User.ID); !errors.Is(err, store.ErrInviteInvalid) {
		t.Fatalf("expired: %v", err)
	}
	if list, _ := s.Invites(ctx, org.ID); len(list) != 0 {
		t.Fatalf("expired invite listed: %+v", list)
	}

	revoked, rinv, err := s.CreateInvite(ctx, org.ID, store.RoleMember, a.User.ID, week)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInvite(ctx, a.PersonalOrg.ID, rinv.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoke via other org: %v", err)
	}
	if err := s.RevokeInvite(ctx, org.ID, rinv.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInvite(ctx, org.ID, rinv.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoke twice: %v", err)
	}
	if _, _, err := s.AcceptInvite(ctx, revoked, c.User.ID); !errors.Is(err, store.ErrInviteInvalid) {
		t.Fatalf("revoked: %v", err)
	}
	if _, _, err := s.AcceptInvite(ctx, "obsinv_nope", c.User.ID); !errors.Is(err, store.ErrInviteInvalid) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := s.Membership(ctx, org.ID, c.User.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("c joined: %v", err)
	}
}

func TestNoInvitesToPersonalOrg(t *testing.T) {
	ctx := context.Background()
	s := store.New(store.NewTestDB(t))
	a := login(t, s, "a")
	if _, _, err := s.CreateInvite(ctx, a.PersonalOrg.ID, store.RoleMember, a.User.ID, week); !errors.Is(err, store.ErrPersonalOrg) {
		t.Fatalf("personal: %v", err)
	}
	if _, _, err := s.CreateInvite(ctx, uuid.New(), store.RoleMember, a.User.ID, week); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing org: %v", err)
	}
}
