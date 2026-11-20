package api

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

type fakeExpiryFleet struct{ dropped, notified []uuid.UUID }

func (f *fakeExpiryFleet) DropMetrics(id uuid.UUID) { f.dropped = append(f.dropped, id) }
func (f *fakeExpiryFleet) NotifyOrg(id uuid.UUID)   { f.notified = append(f.notified, id) }

// expireVM puts delete_at in the past, which the API refuses.
func (e *env) expireVM(id string) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), "UPDATE vms SET delete_at = now() - interval '1 minute' WHERE id = $1", id); err != nil {
		e.t.Fatal(err)
	}
}

func TestVMExpiryDeletesExpired(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	old := e.createVM(tok, org, vmBody("old", userOwner(), node))
	e.createVM(tok, org, withDeleteAfter(vmBody("later", userOwner(), node), "1h"))
	e.createVM(tok, org, vmBody("never", userOwner(), node))
	e.expireVM(old.ID)

	f := &fakeExpiryFleet{}
	if err := NewVMExpiry(e.store, e.z, f).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l := e.listVMs(tok, org); len(l.VMs) != 2 {
		t.Fatalf("left: %+v", l.VMs)
	}
	h := e.history(tok, org.String())
	if len(h.VMs) != 1 || h.VMs[0].ID != old.ID || h.VMs[0].Reason != "expired" || h.VMs[0].DeletedBy != nil {
		t.Fatalf("history: %+v", h)
	}
	es := e.audit("vm:delete")
	if len(es) != 1 || es[0].ActorKind != store.ActorSystem || es[0].Details["reason"] != "expired" || es[0].Resource != authz.VMObject(uuid.MustParse(old.ID)) {
		t.Fatalf("audit: %+v", es)
	}
	if len(f.dropped) != 1 || f.dropped[0].String() != old.ID || len(f.notified) != 1 || f.notified[0] != org {
		t.Fatalf("fleet: %+v", f)
	}
}

func TestVMExpirySkipsForkPending(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	src := e.createVM(tok, org, vmBody("src", userOwner(), node))
	other := e.createVM(tok, org, vmBody("other", userOwner(), node))
	fork := e.createVM(tok, org, vmBody("fork", userOwner(), node))
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, "UPDATE vms SET fork_source_vm = $1 WHERE id = $2", src.ID, fork.ID); err != nil {
		t.Fatal(err)
	}
	e.expireVM(src.ID)
	e.expireVM(other.ID)

	x := NewVMExpiry(e.store, e.z, &fakeExpiryFleet{})
	if err := x.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if h := e.history(tok, org.String()); len(h.VMs) != 1 || h.VMs[0].ID != other.ID {
		t.Fatalf("after first tick: %+v", h)
	}
	if _, err := e.pool.Exec(ctx, "UPDATE vms SET provisioned = true WHERE id = $1", fork.ID); err != nil {
		t.Fatal(err)
	}
	if err := x.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if h := e.history(tok, org.String()); len(h.VMs) != 2 {
		t.Fatalf("after fork provisioned: %+v", h)
	}
}
