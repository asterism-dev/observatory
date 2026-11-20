package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

type expiryFleet interface {
	NotifyOrg(uuid.UUID)
}

// VMExpiry deletes VMs whose delete-after time has passed.
type VMExpiry struct {
	store *store.Store
	z     *authz.Service
	fleet expiryFleet

	Now func() time.Time
}

func NewVMExpiry(s *store.Store, z *authz.Service, f expiryFleet) *VMExpiry {
	return &VMExpiry{store: s, z: z, fleet: f, Now: time.Now}
}

func (x *VMExpiry) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := x.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.Error("vm expiry", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick leaves a VM a pending fork still copies from for a later tick.
func (x *VMExpiry) Tick(ctx context.Context) error {
	vms, err := x.store.ExpiredVMs(ctx, x.Now())
	if err != nil {
		return err
	}
	deleted := false
	for _, v := range vms {
		if x.expire(ctx, v) {
			deleted = true
		}
	}
	// One reload covers every VM deleted in this pass.
	if deleted {
		if err := x.z.Reload(ctx); err != nil {
			slog.Error("vm expiry: reload policies", "err", err)
		}
	}
	return nil
}

// expire reports whether it deleted v.
func (x *VMExpiry) expire(ctx context.Context, v store.VM) bool {
	err := x.store.InTx(ctx, func(tx *store.Store) error {
		if err := tx.LockExpiredVM(ctx, v.ID, x.Now()); err != nil {
			return err
		}
		if err := tx.DeleteVM(ctx, v.OrgID, v.ID, nil, store.DeleteReasonExpired); err != nil {
			return err
		}
		return tx.Audit(ctx, store.AuditEntry{ActorKind: store.ActorSystem, OrgID: &v.OrgID, Action: store.ActionVMDelete,
			Resource: authz.VMObject(v.ID), Result: store.ResultAllowed, Details: map[string]any{"name": v.Name, "reason": store.DeleteReasonExpired}})
	})
	if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		slog.Error("vm expiry", "vm", v.ID, "err", err)
		return false
	}
	x.fleet.NotifyOrg(v.OrgID)
	return true
}
