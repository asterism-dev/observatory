package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/netaddr"
)

const (
	MinGuestMTU = 1000
	// Guest frames ride Geneve inside wg-underlay, so a larger MTU would not fit.
	MaxGuestMTU = netaddr.UnderlayMTU - netaddr.GeneveOverhead
)

var ErrGuestMTURange = errors.New("guest MTU out of range")

func (s *Store) GuestMTU(ctx context.Context) (int, error) {
	var mtu int
	err := s.q.QueryRow(ctx, "SELECT guest_mtu FROM platform_settings").Scan(&mtu)
	return mtu, err
}

func (s *Store) SetGuestMTU(ctx context.Context, mtu int, by uuid.UUID) error {
	if mtu < MinGuestMTU || mtu > MaxGuestMTU {
		return fmt.Errorf("%w: %d-%d", ErrGuestMTURange, MinGuestMTU, MaxGuestMTU)
	}
	return s.InTx(ctx, func(tx *Store) error {
		if _, err := tx.q.Exec(ctx, "UPDATE platform_settings SET guest_mtu = $1", mtu); err != nil {
			return err
		}
		return tx.Audit(ctx, AuditEntry{ActorKind: ActorUser, ActorID: by.String(), Action: ActionPlatformGuestMTU,
			Resource: "admin/network", Result: ResultAllowed, Details: map[string]any{"guestMTU": mtu}})
	})
}
