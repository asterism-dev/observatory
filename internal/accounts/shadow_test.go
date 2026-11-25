package accounts

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

func TestShadowClassifies(t *testing.T) {
	f := newFixture()
	teamVM, otherVM := uuid.New(), uuid.New()
	f.snap.Resources["vm/"+teamVM.String()] = f.ml
	f.snap.Resources["vm/"+otherVM.String()] = f.def
	sh := NewShadow(func(context.Context) (store.AccountSnapshot, error) { return f.snap, nil })
	sh.Reload(context.Background(), f.policy)
	member, teamMember := f.member, f.teamAdmin
	for _, c := range []struct {
		user     uuid.UUID
		obj, act string
		old      bool
		want     Outcome
	}{
		{member, "org/vms/" + teamVM.String(), "vm:connect", false, Same},
		{teamMember, "org/vms/" + teamVM.String(), "vm:connect", true, Same},
		{member, "org/vms/" + otherVM.String(), "vm:connect", false, Widening},
		{member, "org/vms/" + teamVM.String(), "vm:power", true, Regression},
		{member, "org/members", "member:read", true, Skipped},
	} {
		if got := sh.Compare(c.user, f.org, c.obj, c.act, c.old); got != c.want {
			t.Errorf("Compare(%s %s old=%v) = %v, want %v", c.obj, c.act, c.old, got, c.want)
		}
	}
	if n := sh.Counts()[Regression]; n != 1 {
		t.Errorf("regression count = %d", n)
	}
}

func TestShadowNeverFailsRequest(t *testing.T) {
	sh := NewShadow(func(context.Context) (store.AccountSnapshot, error) {
		return store.AccountSnapshot{}, errors.New("db down")
	})
	sh.Reload(context.Background(), authz.Policy{})
	if got := sh.Compare(uuid.New(), uuid.New(), "org/vms/"+uuid.NewString(), "vm:power", true); got != Skipped {
		t.Fatalf("Compare without state = %v, want Skipped", got)
	}
}
