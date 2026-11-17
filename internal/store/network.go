package store

import (
	"context"
	"errors"
	"net/netip"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/seal"
)

// addressLock serializes every hub-wide address allocation; take it after any row lock, never before.
const addressLock int64 = 0x6f627376_6e657400

var (
	underlayFirst = netip.MustParseAddr("10.198.0.10")
	underlayLast  = netip.MustParseAddr("10.198.255.254")
)

var ErrAddressesExhausted = errors.New("no free addresses left")

func exhausted(err error) error {
	if errors.Is(err, netaddr.ErrExhausted) {
		return ErrAddressesExhausted
	}
	return err
}

func (s *Store) lockAddresses(ctx context.Context) error {
	_, err := s.q.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", addressLock)
	return err
}

func (s *Store) collectAddrs(ctx context.Context, sql string, args ...any) ([]netip.Addr, error) {
	rows, err := s.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[netip.Addr])
}

// OrgSlice returns the org's management slice, allocating it on first use; slices are never reused while the org exists.
func (s *Store) OrgSlice(ctx context.Context, org uuid.UUID) (netip.Prefix, error) {
	var slice *netip.Prefix
	err := s.q.QueryRow(ctx, "SELECT mgmt_slice FROM orgs WHERE id = $1", org).Scan(&slice)
	if errors.Is(err, pgx.ErrNoRows) {
		return netip.Prefix{}, ErrNotFound
	}
	if err != nil || slice != nil {
		return deref(slice), err
	}
	err = s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockOrg(ctx, org); err != nil {
			return err
		}
		if err := tx.q.QueryRow(ctx, "SELECT mgmt_slice FROM orgs WHERE id = $1", org).Scan(&slice); err != nil || slice != nil {
			return err
		}
		if err := tx.lockAddresses(ctx); err != nil {
			return err
		}
		rows, err := tx.q.Query(ctx, "SELECT mgmt_slice FROM orgs WHERE mgmt_slice IS NOT NULL")
		if err != nil {
			return err
		}
		used, err := pgx.CollectRows(rows, pgx.RowTo[netip.Prefix])
		if err != nil {
			return err
		}
		fresh, err := netaddr.FreeSlice(used)
		if err != nil {
			return exhausted(err)
		}
		slice = &fresh
		_, err = tx.q.Exec(ctx, "UPDATE orgs SET mgmt_slice = $2 WHERE id = $1", org, fresh)
		return err
	})
	return deref(slice), err
}

func deref(p *netip.Prefix) netip.Prefix {
	if p == nil {
		return netip.Prefix{}
	}
	return *p
}

// nextMgmtIP needs the org row locked so two VMs of the org can't pick the same address.
func (s *Store) nextMgmtIP(ctx context.Context, org uuid.UUID) (netip.Addr, error) {
	slice, err := s.OrgSlice(ctx, org)
	if err != nil {
		return netip.Addr{}, err
	}
	used, err := s.collectAddrs(ctx, "SELECT mgmt_ip FROM vms WHERE org_id = $1 AND mgmt_ip IS NOT NULL", org)
	if err != nil {
		return netip.Addr{}, err
	}
	ip, err := netaddr.FreeHost(slice, used)
	return ip, exhausted(err)
}

// BackfillMgmtIPs gives VMs created before management addresses existed one.
func (s *Store) BackfillMgmtIPs(ctx context.Context) (int, error) {
	rows, err := s.q.Query(ctx, "SELECT id, org_id FROM vms WHERE mgmt_ip IS NULL")
	if err != nil {
		return 0, err
	}
	type pending struct{ id, org uuid.UUID }
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (p pending, err error) {
		err = r.Scan(&p.id, &p.org)
		return
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range list {
		err := s.InTx(ctx, func(tx *Store) error {
			if err := tx.LockOrg(ctx, p.org); err != nil {
				return err
			}
			ip, err := tx.nextMgmtIP(ctx, p.org)
			if err != nil {
				return err
			}
			tag, err := tx.q.Exec(ctx, "UPDATE vms SET mgmt_ip = $2 WHERE id = $1 AND mgmt_ip IS NULL", p.id, ip)
			n += int(tag.RowsAffected())
			return err
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// EnrollChassis stores the node's current chassis key and returns its underlay address, which never changes once set.
func (s *Store) EnrollChassis(ctx context.Context, node uuid.UUID, publicKey string) (netip.Addr, error) {
	var ip netip.Addr
	err := s.InTx(ctx, func(tx *Store) error {
		// NO KEY UPDATE so a concurrent CreateVM's foreign-key check on this node doesn't deadlock with us.
		err := tx.q.QueryRow(ctx, "SELECT underlay_ip FROM nodes WHERE id = $1 FOR NO KEY UPDATE", node).Scan(&ip)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// The address lock also serializes against CreateDevice, which shares the WireGuard key space on the gateway.
		if err := tx.lockAddresses(ctx); err != nil {
			return err
		}
		if err := tx.checkKeyFree(ctx, publicKey, node); err != nil {
			return err
		}
		if !ip.IsValid() {
			used, err := tx.collectAddrs(ctx, "SELECT underlay_ip FROM nodes WHERE underlay_ip IS NOT NULL")
			if err != nil {
				return err
			}
			if ip, err = netaddr.FreeAddr(underlayFirst, underlayLast, used); err != nil {
				return exhausted(err)
			}
		}
		_, err = tx.q.Exec(ctx, "UPDATE nodes SET chassis_key = $2, underlay_ip = $3 WHERE id = $1", node, publicKey, ip)
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
	return ip, err
}

// checkKeyFree fails with ErrConflict if a device or another node's chassis already uses the WireGuard key.
func (s *Store) checkKeyFree(ctx context.Context, key string, node uuid.UUID) error {
	var taken bool
	err := s.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM devices WHERE public_key = $1)
		OR EXISTS (SELECT 1 FROM nodes WHERE chassis_key = $1 AND id <> $2)`, key, node).Scan(&taken)
	if err == nil && taken {
		err = ErrConflict
	}
	return err
}

func hubSecretAAD(name string) []byte { return []byte("hub-secret:" + name) }

// HubSecret returns the named secret, creating it with generate on first use; racing hubs converge on the first insert.
func (s *Store) HubSecret(ctx context.Context, name string, generate func() ([]byte, error)) ([]byte, error) {
	if s.master == nil {
		return nil, ErrNoMasterKey
	}
	load := func() ([]byte, error) {
		var keyID string
		var nonce, sealed []byte
		if err := s.q.QueryRow(ctx, "SELECT key_id, nonce, sealed FROM hub_secrets WHERE name = $1", name).Scan(&keyID, &nonce, &sealed); err != nil {
			return nil, err
		}
		if keyID != s.master.ID() {
			return nil, ErrMasterKeyMismatch
		}
		return seal.Open(*s.master, nonce, sealed, hubSecretAAD(name))
	}
	v, err := load()
	if !errors.Is(err, pgx.ErrNoRows) {
		return v, err
	}
	plain, err := generate()
	if err != nil {
		return nil, err
	}
	nonce, sealed, err := seal.Seal(*s.master, plain, hubSecretAAD(name))
	if err != nil {
		return nil, err
	}
	if _, err := s.q.Exec(ctx, `
		INSERT INTO hub_secrets (name, key_id, nonce, sealed) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
		name, s.master.ID(), nonce, sealed); err != nil {
		return nil, err
	}
	return load()
}

type OrgNet struct {
	ID    uuid.UUID
	Slice netip.Prefix
}

// Chassis is an enrolled node's underlay identity.
type Chassis struct {
	NodeID, OrgID uuid.UUID
	PublicKey     string
	IP            netip.Addr
}

// NetSnapshot is everything the gateway state is built from, read at one point in time and sorted by id (devices by IP).
type NetSnapshot struct {
	Orgs     []OrgNet
	Chassis  []Chassis
	Devices  []Device
	VMs      []VM
	Networks []Network
	NICs     []VMNIC
}

func (s *Store) Chassis(ctx context.Context) ([]Chassis, error) { return readChassis(ctx, s.q) }

func readChassis(ctx context.Context, q querier) ([]Chassis, error) {
	rows, err := q.Query(ctx, "SELECT id, org_id, chassis_key, underlay_ip FROM nodes WHERE chassis_key IS NOT NULL AND underlay_ip IS NOT NULL ORDER BY id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (c Chassis, err error) {
		err = r.Scan(&c.NodeID, &c.OrgID, &c.PublicKey, &c.IP)
		return
	})
}

// NetworkSnapshot reads in one read-only transaction; inside a transaction it uses that one.
func (s *Store) NetworkSnapshot(ctx context.Context) (NetSnapshot, error) {
	pool, ok := s.q.(*pgxpool.Pool)
	if !ok {
		return readNetSnapshot(ctx, s.q)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return NetSnapshot{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return readNetSnapshot(ctx, tx)
}

func readNetSnapshot(ctx context.Context, q querier) (snap NetSnapshot, err error) {
	rows, err := q.Query(ctx, "SELECT id, mgmt_slice FROM orgs WHERE mgmt_slice IS NOT NULL ORDER BY id")
	if err != nil {
		return snap, err
	}
	if snap.Orgs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (o OrgNet, err error) {
		err = r.Scan(&o.ID, &o.Slice)
		return
	}); err != nil {
		return snap, err
	}
	if snap.Chassis, err = readChassis(ctx, q); err != nil {
		return snap, err
	}
	rows, err = q.Query(ctx, "SELECT "+deviceColumns+" FROM devices ORDER BY ip")
	if err != nil {
		return snap, err
	}
	if snap.Devices, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Device, error) { return scanDevice(r) }); err != nil {
		return snap, err
	}
	if snap.VMs, err = collectVMs(q.Query(ctx, "SELECT "+vmColumns+" FROM vms WHERE mgmt_ip IS NOT NULL ORDER BY id")); err != nil {
		return snap, err
	}
	rows, err = q.Query(ctx, "SELECT "+networkColumns+" FROM networks ORDER BY id")
	if err != nil {
		return snap, err
	}
	if snap.Networks, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Network, error) { return scanNetwork(r) }); err != nil {
		return snap, err
	}
	rows, err = q.Query(ctx, "SELECT vm_id, network_id, ip FROM vm_networks ORDER BY vm_id, network_id")
	if err != nil {
		return snap, err
	}
	snap.NICs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (n VMNIC, err error) {
		err = r.Scan(&n.VMID, &n.NetworkID, &n.IP)
		return
	})
	return snap, err
}
