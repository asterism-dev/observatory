package store_test

import (
	"context"
	"testing"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/store"
)

func TestGuestMTUDefault(t *testing.T) {
	s := store.New(dbtest.New(t))
	if got, err := s.GuestMTU(context.Background()); err != nil || got != 1300 {
		t.Fatalf("got %d, %v", got, err)
	}
}

func TestSetGuestMTURange(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t))
	u := login(t, s, "a").User
	for _, bad := range []int{999, 1401} {
		if err := s.SetGuestMTU(ctx, bad, u.ID); err == nil {
			t.Fatalf("%d accepted", bad)
		}
	}
	if err := s.SetGuestMTU(ctx, 1400, u.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GuestMTU(ctx); got != 1400 {
		t.Fatalf("got %d", got)
	}
	es, err := s.AuditEntries(ctx, store.AuditFilter{Action: store.ActionPlatformGuestMTU})
	if err != nil || len(es) != 1 || es[0].ActorID != u.ID.String() || es[0].Details["guestMTU"] != float64(1400) {
		t.Fatalf("audit: %+v %v", es, err)
	}
}
