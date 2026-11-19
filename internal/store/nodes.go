package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/asterism-dev/observatory/internal/proto"
)

var ErrEnrollInvalid = errors.New("invalid enroll token")

const enrollTTL = 24 * time.Hour

type Node struct {
	ID, OrgID  uuid.UUID
	Name       string
	Enrolled   bool
	CPUs       int
	MemoryMB   int
	Machine    json.RawMessage
	Version    string
	LastSeen   *time.Time
	UnderlayIP netip.Addr
	// Quarantined is set by the gateway and cleared only by re-enrollment.
	Quarantined bool
	CreatedBy   uuid.UUID
	CreatedAt   time.Time
	Image       *proto.ImageStatus
	Hypervisor  *proto.Hypervisor
}

const nodeColumns = "id, org_id, name, enrolled_at IS NOT NULL, coalesce(cpus, 0), coalesce(memory_mb, 0), machine, coalesce(version, ''), last_seen, underlay_ip, quarantined, coalesce(created_by, '00000000-0000-0000-0000-000000000000'::uuid), created_at, image, hypervisor"

func scanNode(row pgx.Row) (n Node, err error) {
	err = row.Scan(&n.ID, &n.OrgID, &n.Name, &n.Enrolled, &n.CPUs, &n.MemoryMB, &n.Machine, &n.Version, &n.LastSeen, &n.UnderlayIP, &n.Quarantined, &n.CreatedBy, &n.CreatedAt, &n.Image, &n.Hypervisor)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

func randomSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (s *Store) CreateNode(ctx context.Context, n Node) (Node, string, error) {
	token, err := randomSecret()
	if err != nil {
		return Node{}, "", err
	}
	out, err := scanNode(s.q.QueryRow(ctx, `
		INSERT INTO nodes (org_id, name, enroll_hash, enroll_expires, created_by)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4), $5)
		RETURNING `+nodeColumns, n.OrgID, n.Name, hashToken(token), enrollTTL.Seconds(), n.CreatedBy))
	if isUniqueViolation(err) {
		return Node{}, "", ErrConflict
	}
	if err != nil {
		return Node{}, "", err
	}
	return out, token, nil
}

func (s *Store) Nodes(ctx context.Context, org uuid.UUID) ([]Node, error) {
	rows, err := s.q.Query(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE org_id = $1 ORDER BY name", org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Node, error) { return scanNode(r) })
}

func (s *Store) Node(ctx context.Context, org, id uuid.UUID) (Node, error) {
	return scanNode(s.q.QueryRow(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id = $1 AND org_id = $2", id, org))
}

// EnrollNode consumes the enroll token and issues a new credential; an already-enrolled node keeps its id and VMs.
func (s *Store) EnrollNode(ctx context.Context, token string) (Node, string, error) {
	secret, err := randomSecret()
	if err != nil {
		return Node{}, "", err
	}
	n, err := scanNode(s.q.QueryRow(ctx, `
		UPDATE nodes SET credential_hash = $2, enroll_hash = NULL, enroll_expires = NULL, enrolled_at = coalesce(enrolled_at, now()), quarantined = false
		WHERE enroll_hash = $1 AND enroll_expires > now()
		RETURNING `+nodeColumns, hashToken(token), hashToken(secret)))
	if errors.Is(err, ErrNotFound) {
		return Node{}, "", ErrEnrollInvalid
	}
	if err != nil {
		return Node{}, "", err
	}
	return n, n.ID.String() + "." + secret, nil
}

func (s *Store) NodeByCredential(ctx context.Context, credential string) (Node, error) {
	idPart, secret, ok := strings.Cut(credential, ".")
	id, err := uuid.Parse(idPart)
	if !ok || err != nil {
		return Node{}, ErrNotFound
	}
	var hash []byte
	err = s.q.QueryRow(ctx, "SELECT credential_hash FROM nodes WHERE id = $1", id).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, err
	}
	sum := sha256.Sum256([]byte(secret))
	if hash == nil || subtle.ConstantTimeCompare(hash, sum[:]) != 1 {
		return Node{}, ErrNotFound
	}
	return scanNode(s.q.QueryRow(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id = $1", id))
}

// RegenerateNodeCredential revokes the credential and issues a fresh enroll token; the node stays enrolled (offline).
func (s *Store) RegenerateNodeCredential(ctx context.Context, org, id uuid.UUID) (string, error) {
	token, err := randomSecret()
	if err != nil {
		return "", err
	}
	tag, err := s.q.Exec(ctx, `
		UPDATE nodes SET credential_hash = NULL, enroll_hash = $3, enroll_expires = now() + make_interval(secs => $4)
		WHERE id = $1 AND org_id = $2`, id, org, hashToken(token), enrollTTL.Seconds())
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", ErrNotFound
	}
	return token, nil
}

// UpdateNodeInfo stores what the node registered with; a nil hypervisor clears it.
func (s *Store) UpdateNodeInfo(ctx context.Context, id uuid.UUID, cpus, memoryMB int, machine json.RawMessage, version string, hv *proto.Hypervisor) error {
	tag, err := s.q.Exec(ctx, `
		UPDATE nodes SET cpus = $2, memory_mb = $3, machine = $4, version = $5, hypervisor = $6, last_seen = now() WHERE id = $1`,
		id, cpus, memoryMB, machine, version, hv)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteNode(ctx context.Context, org, id uuid.UUID) error {
	tag, err := s.q.Exec(ctx, "DELETE FROM nodes WHERE id = $1 AND org_id = $2", id, org)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
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

func (s *Store) RenameNode(ctx context.Context, org, id uuid.UUID, name string) error {
	tag, err := s.q.Exec(ctx, "UPDATE nodes SET name = $3 WHERE org_id = $1 AND id = $2", org, id, name)
	switch {
	case isUniqueViolation(err):
		return ErrConflict
	case err != nil:
		return err
	case tag.RowsAffected() == 0:
		return ErrNotFound
	}
	return nil
}

// QuarantineNodes marks the nodes quarantined and returns those that were not already.
func (s *Store) QuarantineNodes(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.q.Query(ctx, "UPDATE nodes SET quarantined = true WHERE id = ANY($1) AND NOT quarantined RETURNING id", ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

func (s *Store) SetNodeImage(ctx context.Context, id uuid.UUID, img proto.ImageStatus) error {
	tag, err := s.q.Exec(ctx, "UPDATE nodes SET image = $2 WHERE id = $1", id, img)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
