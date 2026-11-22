package store

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/catalog"
)

// PutCatalog stores c as caller's catalog; when its digest changed it audits the added and removed action names in the same transaction.
func (s *Store) PutCatalog(ctx context.Context, caller string, c authz.Catalog) (digest string, changed bool, err error) {
	digest = catalog.Digest(c)
	pc := catalog.ToProto(catalog.Normalize(c))
	payload, err := proto.Marshal(pc)
	if err != nil {
		return "", false, err
	}
	err = s.InTx(ctx, func(tx *Store) error {
		// The placeholder row gives the first registration a row to lock, so concurrent ones serialize.
		if _, err := tx.q.Exec(ctx, `INSERT INTO catalogs (service, digest, payload) VALUES ($1, '', '') ON CONFLICT DO NOTHING`, caller); err != nil {
			return err
		}
		var oldDigest string
		var oldPayload []byte
		if err := tx.q.QueryRow(ctx, `SELECT digest, payload FROM catalogs WHERE service = $1 FOR UPDATE`, caller).Scan(&oldDigest, &oldPayload); err != nil {
			return err
		}
		if oldDigest == digest {
			return nil
		}
		old := &iamv1.Catalog{}
		if err := proto.Unmarshal(oldPayload, old); err != nil {
			return err
		}
		if _, err := tx.q.Exec(ctx, `UPDATE catalogs SET digest = $2, payload = $3, updated_at = now() WHERE service = $1`, caller, digest, payload); err != nil {
			return err
		}
		changed = true
		was, now := actionNames(old.GetActions()), actionNames(pc.GetActions())
		return tx.Audit(ctx, AuditEntry{ActorKind: ActorService, ActorID: caller, Action: ActionCatalogUpdate, Resource: "catalog/" + caller, Result: ResultAllowed,
			Details: map[string]any{"added": missingFrom(now, was), "removed": missingFrom(was, now), "old_digest": oldDigest, "new_digest": digest}})
	})
	if err != nil {
		return "", false, err
	}
	return digest, changed, nil
}

func actionNames(as []*iamv1.CatalogAction) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.GetName())
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// missingFrom lists the names of a that b lacks; both are sorted.
func missingFrom(a, b []string) []string {
	out := []string{}
	for _, n := range a {
		if _, found := slices.BinarySearch(b, n); !found {
			out = append(out, n)
		}
	}
	return out
}

// ponytail: stored rows are not re-validated against current prefixes; matters when an extraction drops a prefix, re-registration fixes it.
func readCatalogs(ctx context.Context, q querier) ([]authz.Catalog, error) {
	rows, err := q.Query(ctx, `SELECT payload FROM catalogs WHERE digest <> '' ORDER BY service`)
	if err != nil {
		return nil, err
	}
	payloads, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return nil, err
	}
	out := make([]authz.Catalog, 0, len(payloads))
	for _, b := range payloads {
		pc := &iamv1.Catalog{}
		if err := proto.Unmarshal(b, pc); err != nil {
			return nil, err
		}
		c, err := catalog.FromProto(pc)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}
