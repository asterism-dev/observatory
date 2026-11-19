package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

func TestNodeLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	a, b := login(t, s, "a"), login(t, s, "b")
	orgA, _ := s.CreateOrg(ctx, "A", a.User.ID)
	orgB, _ := s.CreateOrg(ctx, "B", b.User.ID)

	n, token, err := s.CreateNode(ctx, store.Node{OrgID: orgA.ID, Name: "n1", CreatedBy: a.User.ID})
	if err != nil || n.Enrolled {
		t.Fatalf("create: %+v %v", n, err)
	}
	if _, _, err := s.CreateNode(ctx, store.Node{OrgID: orgA.ID, Name: "n1", CreatedBy: a.User.ID}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup: %v", err)
	}
	if _, err := s.Node(ctx, orgB.ID, n.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-org: %v", err)
	}

	enrolled, cred, err := s.EnrollNode(ctx, token)
	if err != nil || enrolled.ID != n.ID || !enrolled.Enrolled || !strings.HasPrefix(cred, n.ID.String()+".") {
		t.Fatalf("enroll: %+v %q %v", enrolled, cred, err)
	}
	if _, _, err := s.EnrollNode(ctx, token); !errors.Is(err, store.ErrEnrollInvalid) {
		t.Fatalf("reuse: %v", err)
	}
	if got, err := s.NodeByCredential(ctx, cred); err != nil || got.ID != n.ID {
		t.Fatalf("credential: %+v %v", got, err)
	}
	if _, err := s.NodeByCredential(ctx, n.ID.String()+".wrong"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("wrong secret: %v", err)
	}

	other, otherToken, _ := s.CreateNode(ctx, store.Node{OrgID: orgA.ID, Name: "n2", CreatedBy: a.User.ID})
	_, otherCred, _ := s.EnrollNode(ctx, otherToken)
	secret := otherCred[len(other.ID.String()):]
	if _, err := s.NodeByCredential(ctx, n.ID.String()+secret); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other node's secret: %v", err)
	}

	token2, err := s.RegenerateNodeCredential(ctx, orgA.ID, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.NodeByCredential(ctx, cred); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old credential: %v", err)
	}
	if got, _ := s.Node(ctx, orgA.ID, n.ID); !got.Enrolled {
		t.Fatal("regenerate must keep Enrolled")
	}
	if again, _, err := s.EnrollNode(ctx, token2); err != nil || again.ID != n.ID {
		t.Fatalf("re-enroll: %+v %v", again, err)
	}
	if _, err := s.RegenerateNodeCredential(ctx, orgB.ID, n.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-org regenerate: %v", err)
	}

	hv := &proto.Hypervisor{Type: "firecracker", Version: "v1.18.0"}
	if err := s.UpdateNodeInfo(ctx, n.ID, 8, 16384, []byte(`{"arch":"amd64"}`), "v1", hv); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Node(ctx, orgA.ID, n.ID); got.CPUs != 8 || got.MemoryMB != 16384 || got.Version != "v1" || got.LastSeen == nil || !strings.Contains(string(got.Machine), "amd64") || got.Hypervisor == nil || *got.Hypervisor != *hv {
		t.Fatalf("info: %+v", got)
	}
}

func TestEnrollTokenExpires(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	a := login(t, s, "a")
	org, _ := s.CreateOrg(ctx, "A", a.User.ID)
	n, token, _ := s.CreateNode(ctx, store.Node{OrgID: org.ID, Name: "n", CreatedBy: a.User.ID})
	var ttlHours float64
	if err := pool.QueryRow(ctx, "SELECT extract(epoch FROM enroll_expires - now()) / 3600 FROM nodes WHERE id = $1", n.ID).Scan(&ttlHours); err != nil || ttlHours < 23.9 || ttlHours > 24 {
		t.Fatalf("ttl %v %v", ttlHours, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE nodes SET enroll_expires = now() - interval '1 second' WHERE id = $1", n.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnrollNode(ctx, token); !errors.Is(err, store.ErrEnrollInvalid) {
		t.Fatalf("expired: %v", err)
	}
}

func TestDeleteNodeWithVM(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	a := login(t, s, "a")
	org, _ := s.CreateOrg(ctx, "A", a.User.ID)
	n, _, _ := s.CreateNode(ctx, store.Node{OrgID: org.ID, Name: "n", CreatedBy: a.User.ID})
	if _, err := pool.Exec(ctx, "INSERT INTO vms (org_id, node_id, user_id, name, cpus, memory_mb, disk_gb) VALUES ($1, $2, $3, 'v', 1, 512, 5)", org.ID, n.ID, a.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNode(ctx, org.ID, n.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("with vm: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM vms"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNode(ctx, org.ID, n.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNode(ctx, org.ID, n.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("again: %v", err)
	}
}

func TestSetNodeImage(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t))
	a, b := login(t, s, "a"), login(t, s, "b")
	orgA, _ := s.CreateOrg(ctx, "A", a.User.ID)
	orgB, _ := s.CreateOrg(ctx, "B", b.User.ID)
	n, _, _ := s.CreateNode(ctx, store.Node{OrgID: orgA.ID, Name: "n1", CreatedBy: a.User.ID})
	other, _, _ := s.CreateNode(ctx, store.Node{OrgID: orgB.ID, Name: "n1", CreatedBy: b.User.ID})
	if got, _ := s.Node(ctx, orgA.ID, n.ID); got.Image != nil {
		t.Fatalf("image before report: %+v", got.Image)
	}
	for _, st := range []string{proto.ImageBuilding, proto.ImageReady} {
		if err := s.SetNodeImage(ctx, n.ID, proto.ImageStatus{Version: "v1", State: st}); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.Node(ctx, orgA.ID, n.ID); got.Image == nil || got.Image.State != proto.ImageReady || got.Image.Version != "v1" {
		t.Fatalf("image: %+v", got.Image)
	}
	if got, _ := s.Node(ctx, orgB.ID, other.ID); got.Image != nil {
		t.Fatalf("other node touched: %+v", got.Image)
	}
}
