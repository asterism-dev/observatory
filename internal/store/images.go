package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrImageExists  = errors.New("an image with this name already exists")
	ErrBuildRunning = errors.New("a build of this image is already queued or running")
)

const (
	BuildQueued    = "queued"
	BuildRunning   = "running"
	BuildSucceeded = "succeeded"
	BuildFailed    = "failed"
	BuildCancelled = "cancelled"

	VersionFromBuild = "build"
	VersionFromPush  = "push"

	buildLogKeep   = 5 << 20
	buildLogMarker = "\n--- log truncated ---\n"
)

// Image of the platform has a nil org in the database; OrgID is then the zero uuid.
type Image struct {
	ID, OrgID      uuid.UUID
	Scope          EnvScope
	OwnerID        *uuid.UUID
	Name           string
	Description    string
	RecipeRevision *int
	Platform       bool
	CreatedBy      uuid.UUID
	CreatedAt      time.Time
}

type RecipeFile struct {
	Path    string
	Mode    uint32
	Content []byte
}

// RecipeRef names a secret (by variable name) or an integration (by kind) the build may use.
type RecipeRef struct {
	Kind    string
	Scope   EnvScope
	OwnerID *uuid.UUID
	Name    string
}

type RecipeRevision struct {
	ImageID   uuid.UUID
	Number    int
	Files     []RecipeFile
	Refs      []RecipeRef
	CreatedBy uuid.UUID
	CreatedAt time.Time
}

type ImageVersion struct {
	ImageID   uuid.UUID
	Digest    string
	Source    string
	Revision  *int
	BuildID   *uuid.UUID
	SizeBytes int64
	CreatedBy *uuid.UUID
	CreatedAt time.Time
}

type ImageBuild struct {
	ID, ImageID           uuid.UUID
	Revision              int
	State                 string
	NodeID, VMID          *uuid.UUID
	StartedBy             uuid.UUID
	QueuedAt              time.Time
	StartedAt, FinishedAt *time.Time
	Digest, Error         string
}

type ImageDefault struct {
	OrgID       uuid.UUID
	Scope       EnvScope
	OwnerID     *uuid.UUID
	ImageID     uuid.UUID
	Tag, Digest string
}

const imageCols = "id, coalesce(org_id, '00000000-0000-0000-0000-000000000000'::uuid), scope, coalesce(team_id, user_id), name, description, recipe_revision, platform, coalesce(created_by, '00000000-0000-0000-0000-000000000000'::uuid), created_at"

func scanImage(r pgx.Row) (v Image, err error) {
	err = r.Scan(&v.ID, &v.OrgID, &v.Scope, &v.OwnerID, &v.Name, &v.Description, &v.RecipeRevision, &v.Platform, &v.CreatedBy, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return v, err
}

func imageOwner(v Image) EnvVar { return EnvVar{OrgID: v.OrgID, Scope: v.Scope, OwnerID: v.OwnerID} }

func (s *Store) CreateImage(ctx context.Context, v Image) (Image, error) {
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.lockEnvOwner(ctx, imageOwner(v)); err != nil {
			return err
		}
		teamID, userID := envOwnerColumns(imageOwner(v))
		var err error
		v, err = scanImage(tx.q.QueryRow(ctx, `
			INSERT INTO images (org_id, scope, team_id, user_id, name, description, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING `+imageCols, v.OrgID, v.Scope, teamID, userID, v.Name, v.Description, v.CreatedBy))
		return err
	})
	if isUniqueViolation(err) {
		return Image{}, ErrImageExists
	}
	return v, err
}

func (s *Store) Image(ctx context.Context, org, id uuid.UUID) (Image, error) {
	return scanImage(s.q.QueryRow(ctx, "SELECT "+imageCols+" FROM images WHERE org_id = $1 AND id = $2", org, id))
}

func (s *Store) ImageByName(ctx context.Context, org uuid.UUID, scope EnvScope, owner *uuid.UUID, name string) (Image, error) {
	teamID, userID := envOwnerColumns(EnvVar{Scope: scope, OwnerID: owner})
	return scanImage(s.q.QueryRow(ctx, "SELECT "+imageCols+` FROM images WHERE org_id = $1 AND scope = $2
		AND coalesce(team_id, user_id) IS NOT DISTINCT FROM coalesce($3::uuid, $4::uuid) AND name = $5`, org, scope, teamID, userID, name))
}

func (s *Store) ListImages(ctx context.Context, org uuid.UUID) ([]Image, error) {
	rows, err := s.q.Query(ctx, "SELECT "+imageCols+" FROM images WHERE org_id = $1 ORDER BY name, id", org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Image, error) { return scanImage(r) })
}

// UpdateImage changes name, description and visibility; versions are the caller's business.
func (s *Store) UpdateImage(ctx context.Context, v Image) (Image, error) {
	var out Image
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.lockEnvOwner(ctx, imageOwner(v)); err != nil {
			return err
		}
		teamID, userID := envOwnerColumns(imageOwner(v))
		var err error
		out, err = scanImage(tx.q.QueryRow(ctx, `
			UPDATE images SET name = $3, description = $4, scope = $5, team_id = $6, user_id = $7 WHERE org_id = $1 AND id = $2
			RETURNING `+imageCols, v.OrgID, v.ID, v.Name, v.Description, v.Scope, teamID, userID))
		return err
	})
	if isUniqueViolation(err) {
		return Image{}, ErrImageExists
	}
	return out, err
}

func (s *Store) DeleteImage(ctx context.Context, org, id uuid.UUID) error {
	tag, err := s.q.Exec(ctx, "DELETE FROM images WHERE org_id = $1 AND id = $2", org, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AddRecipeRevision stores rev as the next revision number and makes it the image's current one.
func (s *Store) AddRecipeRevision(ctx context.Context, rev RecipeRevision) (n int, err error) {
	refs := rev.Refs
	if refs == nil {
		refs = []RecipeRef{}
	}
	refsJSON, err := json.Marshal(refs)
	if err != nil {
		return 0, err
	}
	err = s.InTx(ctx, func(tx *Store) error {
		var cur *int
		if err := tx.q.QueryRow(ctx, "SELECT recipe_revision FROM images WHERE id = $1 FOR UPDATE", rev.ImageID).Scan(&cur); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if err := tx.q.QueryRow(ctx, "SELECT coalesce(max(number), 0) + 1 FROM recipe_revisions WHERE image_id = $1", rev.ImageID).Scan(&n); err != nil {
			return err
		}
		if _, err := tx.q.Exec(ctx, "INSERT INTO recipe_revisions (image_id, number, refs, created_by) VALUES ($1, $2, $3, $4)",
			rev.ImageID, n, refsJSON, rev.CreatedBy); err != nil {
			return err
		}
		for _, f := range rev.Files {
			if _, err := tx.q.Exec(ctx, "INSERT INTO recipe_files (image_id, revision, path, mode, content) VALUES ($1, $2, $3, $4, $5)",
				rev.ImageID, n, f.Path, int64(f.Mode), f.Content); err != nil {
				return err
			}
		}
		_, err := tx.q.Exec(ctx, "UPDATE images SET recipe_revision = $2 WHERE id = $1", rev.ImageID, n)
		return err
	})
	return n, err
}

func (s *Store) RecipeRevision(ctx context.Context, image uuid.UUID, n int) (RecipeRevision, error) {
	rev := RecipeRevision{ImageID: image, Number: n}
	var refs []byte
	err := s.q.QueryRow(ctx, "SELECT refs, created_by, created_at FROM recipe_revisions WHERE image_id = $1 AND number = $2", image, n).
		Scan(&refs, &rev.CreatedBy, &rev.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return rev, ErrNotFound
	} else if err != nil {
		return rev, err
	}
	if err := json.Unmarshal(refs, &rev.Refs); err != nil {
		return rev, err
	}
	rows, err := s.q.Query(ctx, "SELECT path, mode, content FROM recipe_files WHERE image_id = $1 AND revision = $2 ORDER BY path", image, n)
	if err != nil {
		return rev, err
	}
	rev.Files, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (f RecipeFile, err error) {
		var mode int64
		err = r.Scan(&f.Path, &mode, &f.Content)
		f.Mode = uint32(mode)
		return
	})
	return rev, err
}

func (s *Store) AddImageVersion(ctx context.Context, v ImageVersion) error {
	_, err := s.q.Exec(ctx, `
		INSERT INTO image_versions (image_id, digest, source, revision, build_id, size_bytes, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (image_id, digest) DO NOTHING`, v.ImageID, v.Digest, v.Source, v.Revision, v.BuildID, v.SizeBytes, v.CreatedBy)
	return err
}

func (s *Store) ImageVersions(ctx context.Context, image uuid.UUID) ([]ImageVersion, error) {
	rows, err := s.q.Query(ctx, `
		SELECT image_id, digest, source, revision, build_id, size_bytes, created_by, created_at
		FROM image_versions WHERE image_id = $1 ORDER BY created_at DESC, digest`, image)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (v ImageVersion, err error) {
		err = r.Scan(&v.ImageID, &v.Digest, &v.Source, &v.Revision, &v.BuildID, &v.SizeBytes, &v.CreatedBy, &v.CreatedAt)
		return
	})
}

// DeleteImageVersion drops the version's tags and layers with it.
func (s *Store) DeleteImageVersion(ctx context.Context, image uuid.UUID, digest string) error {
	tag, err := s.q.Exec(ctx, "DELETE FROM image_versions WHERE image_id = $1 AND digest = $2", image, digest)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteImageVersions(ctx context.Context, image uuid.UUID) error {
	_, err := s.q.Exec(ctx, "DELETE FROM image_versions WHERE image_id = $1", image)
	return err
}

// SetImageTag points tag at an existing version of the image.
func (s *Store) SetImageTag(ctx context.Context, image uuid.UUID, tag, digest string) error {
	res, err := s.q.Exec(ctx, `
		INSERT INTO image_tags (image_id, tag, digest) SELECT image_id, $2, digest FROM image_versions WHERE image_id = $1 AND digest = $3
		ON CONFLICT (image_id, tag) DO UPDATE SET digest = EXCLUDED.digest`, image, tag, digest)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ImageTags(ctx context.Context, image uuid.UUID) (map[string]string, error) {
	rows, err := s.q.Query(ctx, "SELECT tag, digest FROM image_tags WHERE image_id = $1", image)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var tag, digest string
		if err := rows.Scan(&tag, &digest); err != nil {
			return nil, err
		}
		out[tag] = digest
	}
	return out, rows.Err()
}

func (s *Store) ResolveImageTag(ctx context.Context, image uuid.UUID, tag string) (string, error) {
	var digest string
	err := s.q.QueryRow(ctx, "SELECT digest FROM image_tags WHERE image_id = $1 AND tag = $2", image, tag).Scan(&digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return digest, err
}

const buildCols = "id, image_id, revision, state, node_id, vm_id, started_by, queued_at, started_at, finished_at, digest, error"

func scanBuild(r pgx.Row) (b ImageBuild, err error) {
	err = r.Scan(&b.ID, &b.ImageID, &b.Revision, &b.State, &b.NodeID, &b.VMID, &b.StartedBy, &b.QueuedAt, &b.StartedAt, &b.FinishedAt, &b.Digest, &b.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return b, err
}

func (s *Store) CreateImageBuild(ctx context.Context, b ImageBuild) (ImageBuild, error) {
	b, err := scanBuild(s.q.QueryRow(ctx, `
		INSERT INTO image_builds (image_id, revision, started_by) VALUES ($1, $2, $3) RETURNING `+buildCols, b.ImageID, b.Revision, b.StartedBy))
	if isUniqueViolation(err) {
		return ImageBuild{}, ErrBuildRunning
	}
	return b, err
}

func (s *Store) ImageBuild(ctx context.Context, id uuid.UUID) (ImageBuild, error) {
	return scanBuild(s.q.QueryRow(ctx, "SELECT "+buildCols+" FROM image_builds WHERE id = $1", id))
}

func (s *Store) UpdateImageBuild(ctx context.Context, b ImageBuild) error {
	tag, err := s.q.Exec(ctx, `
		UPDATE image_builds SET state = $2, node_id = $3, vm_id = $4, started_at = $5, finished_at = $6, digest = $7, error = $8 WHERE id = $1`,
		b.ID, b.State, b.NodeID, b.VMID, b.StartedAt, b.FinishedAt, b.Digest, b.Error)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ImageBuilds(ctx context.Context, image uuid.UUID) ([]ImageBuild, error) {
	rows, err := s.q.Query(ctx, "SELECT "+buildCols+" FROM image_builds WHERE image_id = $1 ORDER BY queued_at DESC, id", image)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ImageBuild, error) { return scanBuild(r) })
}

func (s *Store) AppendBuildLog(ctx context.Context, build uuid.UUID, chunk []byte) error {
	_, err := s.q.Exec(ctx, "INSERT INTO image_build_log_chunks (build_id, chunk) VALUES ($1, $2)", build, chunk)
	return err
}

// FinishBuildLog replaces the chunks with one gzip blob, cutting the middle above 2*buildLogKeep.
func (s *Store) FinishBuildLog(ctx context.Context, build uuid.UUID) error {
	return s.InTx(ctx, func(tx *Store) error {
		raw, err := tx.chunkedLog(ctx, build)
		if err != nil {
			return err
		}
		if len(raw) > 2*buildLogKeep {
			raw = append(append(raw[:buildLogKeep:buildLogKeep], buildLogMarker...), raw[len(raw)-buildLogKeep:]...)
		}
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(raw); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		if _, err := tx.q.Exec(ctx, "UPDATE image_builds SET log = $2 WHERE id = $1", build, buf.Bytes()); err != nil {
			return err
		}
		_, err = tx.q.Exec(ctx, "DELETE FROM image_build_log_chunks WHERE build_id = $1", build)
		return err
	})
}

func (s *Store) chunkedLog(ctx context.Context, build uuid.UUID) ([]byte, error) {
	var raw []byte
	err := s.q.QueryRow(ctx, "SELECT coalesce(string_agg(chunk, ''::bytea ORDER BY id), ''::bytea) FROM image_build_log_chunks WHERE build_id = $1", build).Scan(&raw)
	return raw, err
}

// BuildLog returns the finished log, or the chunks so far while the build runs.
func (s *Store) BuildLog(ctx context.Context, build uuid.UUID) ([]byte, error) {
	var gz []byte
	if err := s.q.QueryRow(ctx, "SELECT log FROM image_builds WHERE id = $1", build).Scan(&gz); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if gz == nil {
		return s.chunkedLog(ctx, build)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}

func (s *Store) SetImageDefault(ctx context.Context, d ImageDefault) error {
	return s.InTx(ctx, func(tx *Store) error {
		if err := tx.lockEnvOwner(ctx, EnvVar{OrgID: d.OrgID, Scope: d.Scope, OwnerID: d.OwnerID}); err != nil {
			return err
		}
		teamID, userID := envOwnerColumns(EnvVar{Scope: d.Scope, OwnerID: d.OwnerID})
		_, err := tx.q.Exec(ctx, `
			INSERT INTO image_defaults (org_id, scope, team_id, user_id, image_id, tag, digest) VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (org_id, scope, (coalesce(team_id, user_id, '00000000-0000-0000-0000-000000000000'::uuid)))
			DO UPDATE SET image_id = EXCLUDED.image_id, tag = EXCLUDED.tag, digest = EXCLUDED.digest`,
			d.OrgID, d.Scope, teamID, userID, d.ImageID, d.Tag, d.Digest)
		return err
	})
}

func (s *Store) ClearImageDefault(ctx context.Context, org uuid.UUID, scope EnvScope, owner *uuid.UUID) error {
	teamID, userID := envOwnerColumns(EnvVar{Scope: scope, OwnerID: owner})
	_, err := s.q.Exec(ctx, `DELETE FROM image_defaults WHERE org_id = $1 AND scope = $2
		AND coalesce(team_id, user_id) IS NOT DISTINCT FROM coalesce($3::uuid, $4::uuid)`, org, scope, teamID, userID)
	return err
}

func (s *Store) ImageDefaults(ctx context.Context, org uuid.UUID) ([]ImageDefault, error) {
	rows, err := s.q.Query(ctx, "SELECT org_id, scope, coalesce(team_id, user_id), image_id, tag, digest FROM image_defaults WHERE org_id = $1 ORDER BY scope, image_id", org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (d ImageDefault, err error) {
		err = r.Scan(&d.OrgID, &d.Scope, &d.OwnerID, &d.ImageID, &d.Tag, &d.Digest)
		return
	})
}

// OrgImageUsage sums each layer digest once however many versions share it.
func (s *Store) OrgImageUsage(ctx context.Context, org uuid.UUID) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx, `
		SELECT coalesce(sum(size), 0)::bigint FROM (
			SELECT DISTINCT ON (l.digest) l.size FROM image_version_layers l JOIN images i ON i.id = l.image_id WHERE i.org_id = $1) d`, org).Scan(&n)
	return n, err
}
