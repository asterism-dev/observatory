package store_test

import (
	"context"
	"testing"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/store/storetest"
)

func TestGuestMTUDefault(t *testing.T) {
	s := store.New(dbtest.New(t))
	if got, err := s.GuestMTU(context.Background()); err != nil || got != 1300 {
		t.Fatalf("got %d, %v", got, err)
	}
}

func TestSetGuestMTURange(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	u := login(t, s, "a").User
	for _, bad := range []int{999, 1363} {
		if err := s.SetGuestMTU(ctx, bad, u.ID); err == nil {
			t.Fatalf("%d accepted", bad)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE platform_settings SET guest_mtu = 1363"); err == nil {
		t.Fatal("schema accepted 1363")
	}
	if err := s.SetGuestMTU(ctx, 1362, u.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GuestMTU(ctx); got != 1362 {
		t.Fatalf("got %d", got)
	}
	es := storetest.Audit(t, pool, store.ActionPlatformGuestMTU)
	if len(es) != 1 || es[0].ActorID != u.ID.String() || es[0].Details["guestMTU"] != float64(1362) {
		t.Fatalf("audit: %+v", es)
	}
}
