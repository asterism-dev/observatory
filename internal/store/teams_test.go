package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/store"
)

func TestTeamStore(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t))
	a, b, outsider := login(t, s, "a"), login(t, s, "b"), login(t, s, "c")
	orgA, _ := s.CreateOrg(ctx, "A", a.User.ID)
	orgB, _ := s.CreateOrg(ctx, "B", b.User.ID)

	team, err := s.CreateTeam(ctx, orgA.ID, "core")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeam(ctx, orgA.ID, "core"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := s.CreateTeam(ctx, orgB.ID, "core"); err != nil {
		t.Fatalf("same name in other org: %v", err)
	}
	other, _ := s.CreateTeam(ctx, orgA.ID, "ops")
	if err := s.RenameTeam(ctx, orgA.ID, other.ID, "core"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("rename onto existing: %v", err)
	}

	if err := s.SetTeamMember(ctx, orgA.ID, team.ID, a.User.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamMember(ctx, orgA.ID, team.ID, a.User.ID, false); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := s.SetTeamMember(ctx, orgA.ID, team.ID, outsider.User.ID, false); !errors.Is(err, store.ErrNotMember) {
		t.Fatalf("non-member: %v", err)
	}
	if err := s.SetTeamMember(ctx, orgA.ID, team.ID, b.User.ID, false); !errors.Is(err, store.ErrNotMember) {
		t.Fatalf("member of other org: %v", err)
	}
	if err := s.SetTeamMember(ctx, orgB.ID, team.ID, b.User.ID, false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign team: %v", err)
	}
	got, err := s.Team(ctx, orgA.ID, team.ID)
	if err != nil || got.MemberCount != 1 || got.Name != "core" {
		t.Fatalf("team: %+v %v", got, err)
	}
	if _, err := s.Team(ctx, orgB.ID, team.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign get: %v", err)
	}
	if ms, err := s.TeamMembers(ctx, orgB.ID, team.ID); err != nil || len(ms) != 0 {
		t.Fatalf("foreign members: %+v %v", ms, err)
	}
	if err := s.RemoveTeamMember(ctx, orgB.ID, team.ID, a.User.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign remove: %v", err)
	}
	if err := s.RenameTeam(ctx, orgB.ID, team.ID, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign rename: %v", err)
	}
	if err := s.DeleteTeam(ctx, orgB.ID, team.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign delete: %v", err)
	}
	if err := s.RemoveTeamMember(ctx, orgA.ID, team.ID, a.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveTeamMember(ctx, orgA.ID, team.ID, a.User.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("remove twice: %v", err)
	}
	if ts, err := s.Teams(ctx, orgA.ID); err != nil || len(ts) != 2 || ts[0].Name != "core" {
		t.Fatalf("teams: %+v %v", ts, err)
	}
	if err := s.DeleteTeam(ctx, orgA.ID, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete unknown: %v", err)
	}
	one := 1
	if err := s.SetLimits(ctx, orgA.ID, "team:"+other.ID.String(), store.Limits{VCPU: &one}, policyKnown(t, s, orgA.ID)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTeam(ctx, orgA.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if ls, err := s.Limits(ctx, orgA.ID); err != nil || len(ls) != 0 {
		t.Fatalf("limits after team delete: %+v %v", ls, err)
	}
}
