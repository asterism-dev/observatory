package store_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/store"
)

func mustCreateDevice(t *testing.T, s *store.Store, org, user uuid.UUID, name, key string) store.Device {
	t.Helper()
	d, err := s.CreateDevice(context.Background(), store.Device{OrgID: org, UserID: user, Name: name, PublicKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCreateDevice(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	a, b := login(t, s, "a"), login(t, s, "b")
	orgA := mustCreateOrg(t, s, "A", a.User.ID)
	orgB := mustCreateOrg(t, s, "B", b.User.ID)
	execSQL(t, pool, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'member')`, orgA.ID, b.User.ID)

	d1 := mustCreateDevice(t, s, orgA.ID, a.User.ID, "laptop", "k1")
	d2 := mustCreateDevice(t, s, orgB.ID, b.User.ID, "laptop", "k2")
	if d1.IP != netip.MustParseAddr("10.192.0.2") || d2.IP != netip.MustParseAddr("10.192.0.3") {
		t.Fatalf("ips %s %s", d1.IP, d2.IP)
	}
	var slice *netip.Prefix
	if err := pool.QueryRow(ctx, "SELECT mgmt_slice FROM orgs WHERE id = $1", orgA.ID).Scan(&slice); err != nil || slice == nil {
		t.Fatalf("org A slice %v %v", slice, err)
	}
	if _, err := s.CreateDevice(ctx, store.Device{OrgID: orgA.ID, UserID: a.User.ID, Name: "laptop", PublicKey: "k3"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	stranger := login(t, s, "c")
	if _, err := s.CreateDevice(ctx, store.Device{OrgID: orgA.ID, UserID: stranger.User.ID, Name: "x", PublicKey: "k5"}); !errors.Is(err, store.ErrNotMember) {
		t.Fatalf("non-member: %v", err)
	}
	d3 := mustCreateDevice(t, s, orgA.ID, b.User.ID, "laptop", "k4")
	if d3.IP != netip.MustParseAddr("10.192.0.4") {
		t.Fatalf("d3 ip %s", d3.IP)
	}
	got, err := s.Device(ctx, orgA.ID, d1.ID)
	if err != nil || got.PublicKey != "k1" || got.UserID != a.User.ID || got.IP != d1.IP {
		t.Fatalf("reload %+v %v", got, err)
	}
	list, err := s.Devices(ctx, orgA.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("devices %+v %v", list, err)
	}
	if err := s.DeleteDevice(ctx, orgB.ID, d1.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete via other org: %v", err)
	}
	if err := s.DeleteDevice(ctx, orgA.ID, d1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Device(ctx, orgA.ID, d1.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestRemoveMemberDeletesDevices(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	a, m := login(t, s, "a"), login(t, s, "m")
	orgA := mustCreateOrg(t, s, "A", a.User.ID)
	orgB := mustCreateOrg(t, s, "B", a.User.ID)
	for _, org := range []uuid.UUID{orgA.ID, orgB.ID} {
		execSQL(t, pool, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'member')`, org, m.User.ID)
	}
	inA := mustCreateDevice(t, s, orgA.ID, m.User.ID, "phone", "ka")
	inB := mustCreateDevice(t, s, orgB.ID, m.User.ID, "phone", "kb")

	if err := s.RemoveMember(ctx, orgA.ID, m.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Device(ctx, orgA.ID, inA.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("org A device: %v", err)
	}
	if _, err := s.Device(ctx, orgB.ID, inB.ID); err != nil {
		t.Fatalf("org B device: %v", err)
	}
}

func TestSetDeviceHandshakes(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t))
	a := login(t, s, "a")
	org := mustCreateOrg(t, s, "A", a.User.ID)
	d1 := mustCreateDevice(t, s, org.ID, a.User.ID, "one", "k1")
	d2 := mustCreateDevice(t, s, org.ID, a.User.ID, "two", "k2")
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	if err := s.SetDeviceHandshakes(ctx, map[string]time.Time{"k1": at, "unknown": at}); err != nil {
		t.Fatal(err)
	}
	got1, err := s.Device(ctx, org.ID, d1.ID)
	if err != nil || got1.LastHandshake == nil || !got1.LastHandshake.Equal(at) {
		t.Fatalf("d1 %v %v", got1.LastHandshake, err)
	}
	got2, err := s.Device(ctx, org.ID, d2.ID)
	if err != nil || got2.LastHandshake != nil {
		t.Fatalf("d2 %v %v", got2.LastHandshake, err)
	}
}
