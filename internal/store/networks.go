package store

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/proto"
)

var (
	ErrRestartRequired = errors.New("restart required")
	ErrSubnetOverlap   = errors.New("subnet overlaps another network")
)

type Network struct {
	ID, OrgID uuid.UUID
	Name      string
	Subnet    netip.Prefix
	VMCount   int
	CreatedBy uuid.UUID
	CreatedAt time.Time
}

type VMNIC struct {
	VMID, NetworkID uuid.UUID
	IP              netip.Addr
}

const networkColumns = "id, org_id, name, subnet, (SELECT count(*) FROM vm_networks vn WHERE vn.network_id = networks.id), coalesce(created_by, '00000000-0000-0000-0000-000000000000'::uuid), created_at"

func scanNetwork(row pgx.Row) (n Network, err error) {
	err = row.Scan(&n.ID, &n.OrgID, &n.Name, &n.Subnet, &n.VMCount, &n.CreatedBy, &n.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

func hasPgCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// CreateNetwork picks a free auto subnet when n.Subnet is zero; the exclusion constraint rejects overlaps across all orgs.
func (s *Store) CreateNetwork(ctx context.Context, n Network) (Network, error) {
	subnet := n.Subnet
	if subnet.IsValid() {
		var err error
		if subnet, err = netaddr.ValidateSubnet(subnet.String()); err != nil {
			return Network{}, err
		}
	}
	var out Network
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.lockAddresses(ctx); err != nil {
			return err
		}
		if !subnet.IsValid() {
			rows, err := tx.q.Query(ctx, "SELECT subnet FROM networks")
			if err != nil {
				return err
			}
			used, err := pgx.CollectRows(rows, pgx.RowTo[netip.Prefix])
			if err != nil {
				return err
			}
			if subnet, err = netaddr.FreeSubnet(used); err != nil {
				return exhausted(err)
			}
		}
		var err error
		out, err = scanNetwork(tx.q.QueryRow(ctx, `
			INSERT INTO networks (org_id, name, subnet, created_by) VALUES ($1, $2, $3, $4) RETURNING `+networkColumns,
			n.OrgID, n.Name, subnet, n.CreatedBy))
		switch {
		case hasPgCode(err, "23P01"):
			return ErrSubnetOverlap
		case isUniqueViolation(err):
			return ErrConflict
		}
		return err
	})
	return out, err
}

func (s *Store) Networks(ctx context.Context, org uuid.UUID) ([]Network, error) {
	rows, err := s.q.Query(ctx, "SELECT "+networkColumns+" FROM networks WHERE org_id = $1 ORDER BY name", org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Network, error) { return scanNetwork(r) })
}

func (s *Store) Network(ctx context.Context, org, id uuid.UUID) (Network, error) {
	return scanNetwork(s.q.QueryRow(ctx, "SELECT "+networkColumns+" FROM networks WHERE id = $1 AND org_id = $2", id, org))
}

// DeleteNetwork relies on the vm_networks foreign key, so a concurrent attach can't slip past the check.
func (s *Store) DeleteNetwork(ctx context.Context, org, id uuid.UUID) error {
	tag, err := s.q.Exec(ctx, "DELETE FROM networks WHERE id = $1 AND org_id = $2", id, org)
	if hasPgCode(err, "23503") {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) VMNetworks(ctx context.Context, vm uuid.UUID) ([]VMNIC, error) {
	rows, err := s.q.Query(ctx, "SELECT vm_id, network_id, ip FROM vm_networks WHERE vm_id = $1 ORDER BY network_id", vm)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (n VMNIC, err error) {
		err = r.Scan(&n.VMID, &n.NetworkID, &n.IP)
		return
	})
}

func (s *Store) OrgVMNetworks(ctx context.Context, org uuid.UUID) ([]VMNIC, error) {
	rows, err := s.q.Query(ctx, "SELECT vn.vm_id, vn.network_id, vn.ip FROM vm_networks vn JOIN networks n ON n.id = vn.network_id WHERE n.org_id = $1 ORDER BY vn.vm_id, n.name", org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (n VMNIC, err error) {
		err = r.Scan(&n.VMID, &n.NetworkID, &n.IP)
		return
	})
}

// SetVMNetworks replaces the VM's attachments; retained NICs keep their address.
func (s *Store) SetVMNetworks(ctx context.Context, org, vm uuid.UUID, networks []uuid.UUID, restartAllowed bool) (VM, error) {
	var out VM
	err := s.InTx(ctx, func(tx *Store) error {
		v, err := scanVM(tx.q.QueryRow(ctx, "SELECT "+vmColumns+" FROM vms WHERE id = $1 AND org_id = $2 FOR UPDATE", vm, org))
		if err != nil {
			return err
		}
		out = v
		// Never nil: a NULL array would make "<> ALL" below match nothing.
		want := append([]uuid.UUID{}, networks...)
		slices.SortFunc(want, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
		want = slices.Compact(want)
		type target struct {
			id     uuid.UUID
			subnet netip.Prefix
		}
		// KEY SHARE keeps the networks from being deleted before our rows reference them.
		rows, err := tx.q.Query(ctx, "SELECT id, subnet FROM networks WHERE org_id = $1 AND id = ANY($2) FOR KEY SHARE", org, want)
		if err != nil {
			return err
		}
		found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (t target, err error) {
			err = r.Scan(&t.id, &t.subnet)
			return
		})
		if err != nil {
			return err
		}
		if len(found) != len(want) {
			return ErrNotFound
		}
		current, err := tx.VMNetworks(ctx, vm)
		if err != nil {
			return err
		}
		attached := make(map[uuid.UUID]bool, len(current))
		for _, n := range current {
			attached[n.NetworkID] = true
		}
		var added []target
		for _, t := range found {
			if !attached[t.id] {
				added = append(added, t)
			}
		}
		if len(added) == 0 && len(current) == len(want) {
			return nil
		}
		if v.Power != proto.PowerStopped && !restartAllowed {
			return ErrRestartRequired
		}
		if _, err := tx.q.Exec(ctx, "DELETE FROM vm_networks WHERE vm_id = $1 AND network_id <> ALL($2)", vm, want); err != nil {
			return err
		}
		if len(added) > 0 {
			if err := tx.lockAddresses(ctx); err != nil {
				return err
			}
		}
		for _, t := range added {
			used, err := tx.collectAddrs(ctx, "SELECT ip FROM vm_networks WHERE network_id = $1", t.id)
			if err != nil {
				return err
			}
			ip, err := netaddr.FreeHost(t.subnet, used)
			if err != nil {
				return exhausted(err)
			}
			if _, err := tx.q.Exec(ctx, "INSERT INTO vm_networks (vm_id, network_id, ip) VALUES ($1, $2, $3)", vm, t.id, ip); err != nil {
				return err
			}
		}
		out, err = scanVM(tx.q.QueryRow(ctx, "UPDATE vms SET generation = generation + 1 WHERE id = $1 RETURNING "+vmColumns, vm))
		return err
	})
	return out, err
}
