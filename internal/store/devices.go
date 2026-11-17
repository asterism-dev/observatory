package store

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/asterism-dev/observatory/internal/netaddr"
)

var (
	deviceFirst = netip.MustParseAddr("10.192.0.2")
	deviceLast  = netip.MustParseAddr("10.192.255.254")
)

type Device struct {
	ID, OrgID, UserID uuid.UUID
	Name, PublicKey   string
	IP                netip.Addr
	LastHandshake     *time.Time
	CreatedAt         time.Time
}

const deviceColumns = "id, org_id, user_id, name, public_key, ip, last_handshake, created_at"

func scanDevice(row pgx.Row) (d Device, err error) {
	err = row.Scan(&d.ID, &d.OrgID, &d.UserID, &d.Name, &d.PublicKey, &d.IP, &d.LastHandshake, &d.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

// CreateDevice also allocates the org's management slice, which the device config routes to.
func (s *Store) CreateDevice(ctx context.Context, d Device) (Device, error) {
	if _, err := s.OrgSlice(ctx, d.OrgID); err != nil {
		return Device{}, err
	}
	var out Device
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.lockAddresses(ctx); err != nil {
			return err
		}
		used, err := tx.collectAddrs(ctx, "SELECT ip FROM devices")
		if err != nil {
			return err
		}
		ip, err := netaddr.FreeAddr(deviceFirst, deviceLast, used)
		if err != nil {
			return exhausted(err)
		}
		out, err = scanDevice(tx.q.QueryRow(ctx, `
			INSERT INTO devices (org_id, user_id, name, public_key, ip) VALUES ($1, $2, $3, $4, $5)
			RETURNING `+deviceColumns, d.OrgID, d.UserID, d.Name, d.PublicKey, ip))
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
	return out, err
}

func (s *Store) Devices(ctx context.Context, org uuid.UUID) ([]Device, error) {
	rows, err := s.q.Query(ctx, "SELECT "+deviceColumns+" FROM devices WHERE org_id = $1 ORDER BY name, id", org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Device, error) { return scanDevice(r) })
}

func (s *Store) Device(ctx context.Context, org, id uuid.UUID) (Device, error) {
	return scanDevice(s.q.QueryRow(ctx, "SELECT "+deviceColumns+" FROM devices WHERE id = $1 AND org_id = $2", id, org))
}

func (s *Store) DeleteDevice(ctx context.Context, org, id uuid.UUID) error {
	tag, err := s.q.Exec(ctx, "DELETE FROM devices WHERE id = $1 AND org_id = $2", id, org)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDeviceHandshakes records the latest handshake per public key; unknown keys are ignored.
func (s *Store) SetDeviceHandshakes(ctx context.Context, hs map[string]time.Time) error {
	keys := make([]string, 0, len(hs))
	times := make([]time.Time, 0, len(hs))
	for k, t := range hs {
		keys = append(keys, k)
		times = append(times, t)
	}
	_, err := s.q.Exec(ctx, `
		UPDATE devices d SET last_handshake = h.at
		FROM unnest($1::text[], $2::timestamptz[]) AS h(key, at) WHERE d.public_key = h.key`, keys, times)
	return err
}
