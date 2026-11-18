package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/asterism-dev/observatory/internal/egress"
	"github.com/asterism-dev/observatory/internal/seal"
)

type EgressCA struct {
	ID                  uuid.UUID
	Hosts               []string
	CertPEM, KeyPEM     string
	NotBefore, NotAfter time.Time
}

func (EgressCA) String() string   { return "EgressCA{redacted}" }
func (EgressCA) GoString() string { return "EgressCA{redacted}" }

func egressCAAAD(vm, id uuid.UUID) []byte {
	return []byte("egress-ca:" + vm.String() + ":" + id.String())
}

// EnsureEgressCAs keeps the VM's CAs current for hosts and returns the unexpired ones, oldest first, with keys.
func (s *Store) EnsureEgressCAs(ctx context.Context, org, vm uuid.UUID, hosts []string, now time.Time) (out []EgressCA, err error) {
	hosts, err = egress.NormalizeHosts(hosts)
	if err != nil {
		return nil, err
	}
	err = s.InTx(ctx, func(tx *Store) error {
		var one int
		if err := tx.q.QueryRow(ctx, "SELECT 1 FROM vms WHERE org_id = $1 AND id = $2 FOR UPDATE", org, vm).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if len(hosts) == 0 {
			_, err := tx.q.Exec(ctx, "DELETE FROM vm_egress_cas WHERE vm_id = $1", vm)
			return err
		}
		if _, err := tx.q.Exec(ctx, "DELETE FROM vm_egress_cas WHERE vm_id = $1 AND not_after <= $2", vm, now); err != nil {
			return err
		}
		cas, err := tx.egressCAs(ctx, vm, now)
		if err != nil {
			return err
		}
		fresh := slices.ContainsFunc(cas, func(c EgressCA) bool {
			return c.NotAfter.After(now.Add(egress.CARenewBefore)) && covers(c.Hosts, hosts)
		})
		if !fresh {
			if err := tx.mintEgressCA(ctx, org, vm, hosts, now); err != nil {
				return err
			}
		}
		out, err = tx.openEgressCAs(ctx, org, vm, now)
		return err
	})
	return out, err
}

func covers(have, want []string) bool {
	for _, h := range want {
		if !slices.Contains(have, h) {
			return false
		}
	}
	return true
}

func (s *Store) mintEgressCA(ctx context.Context, org, vm uuid.UUID, hosts []string, now time.Time) error {
	ca, err := egress.NewCA(hosts, now)
	if err != nil {
		return err
	}
	key, err := s.orgKey(ctx, org)
	if err != nil {
		return err
	}
	id := uuid.New()
	nonce, ct, err := seal.Seal(key, []byte(ca.KeyPEM), egressCAAAD(vm, id))
	if err != nil {
		return err
	}
	if _, err := s.q.Exec(ctx, `
		INSERT INTO vm_egress_cas (id, org_id, vm_id, hosts, cert_pem, nonce, key_ciphertext, not_before, not_after)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		id, org, vm, hosts, ca.CertPEM, nonce, ct, ca.NotBefore, ca.NotAfter); err != nil {
		return err
	}
	slog.Info("egress ca minted", "vm", vm, "hosts", hosts, "notAfter", ca.NotAfter)
	return nil
}

func (s *Store) egressCAs(ctx context.Context, vm uuid.UUID, now time.Time) ([]EgressCA, error) {
	rows, err := s.q.Query(ctx, `
		SELECT id, hosts, cert_pem, not_before, not_after FROM vm_egress_cas
		WHERE vm_id = $1 AND not_after > $2 ORDER BY not_after, created_at, id`, vm, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EgressCA
	for rows.Next() {
		var c EgressCA
		if err := rows.Scan(&c.ID, &c.Hosts, &c.CertPEM, &c.NotBefore, &c.NotAfter); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) openEgressCAs(ctx context.Context, org, vm uuid.UUID, now time.Time) ([]EgressCA, error) {
	cas, err := s.egressCAs(ctx, vm, now)
	if err != nil || len(cas) == 0 {
		return nil, err
	}
	key, err := s.orgKey(ctx, org)
	if err != nil {
		return nil, err
	}
	for i, c := range cas {
		var nonce, ct []byte
		if err := s.q.QueryRow(ctx, "SELECT nonce, key_ciphertext FROM vm_egress_cas WHERE id = $1", c.ID).Scan(&nonce, &ct); err != nil {
			return nil, err
		}
		pem, err := seal.Open(key, nonce, ct, egressCAAAD(vm, c.ID))
		if err != nil {
			return nil, fmt.Errorf("open egress ca %s: %w", c.ID, err)
		}
		cas[i].KeyPEM = string(pem)
	}
	return cas, nil
}

// EgressCAInfos returns the VM's unexpired CAs without keys.
func (s *Store) EgressCAInfos(ctx context.Context, vm uuid.UUID, now time.Time) ([]EgressCA, error) {
	return s.egressCAs(ctx, vm, now)
}

// copyEgressCAs gives a fork its own sealed copies of the source's CAs.
func (s *Store) copyEgressCAs(ctx context.Context, org, from, to uuid.UUID) error {
	cas, err := s.openEgressCAs(ctx, org, from, time.Now())
	if err != nil || len(cas) == 0 {
		return err
	}
	key, err := s.orgKey(ctx, org)
	if err != nil {
		return err
	}
	for _, c := range cas {
		id := uuid.New()
		nonce, ct, err := seal.Seal(key, []byte(c.KeyPEM), egressCAAAD(to, id))
		if err != nil {
			return err
		}
		if _, err := s.q.Exec(ctx, `
			INSERT INTO vm_egress_cas (id, org_id, vm_id, hosts, cert_pem, nonce, key_ciphertext, not_before, not_after)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			id, org, to, c.Hosts, c.CertPEM, nonce, ct, c.NotBefore, c.NotAfter); err != nil {
			return err
		}
	}
	return nil
}

// EgressCAOrgs lists orgs with a VM whose newest CA expires within CARenewBefore of now.
func (s *Store) EgressCAOrgs(ctx context.Context, now time.Time) ([]uuid.UUID, error) {
	rows, err := s.q.Query(ctx, `
		SELECT DISTINCT org_id FROM (
			SELECT org_id, vm_id FROM vm_egress_cas GROUP BY org_id, vm_id HAVING max(not_after) < $1) due`,
		now.Add(egress.CARenewBefore))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
