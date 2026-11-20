package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrImageExists      = errors.New("an image with this name already exists")
	ErrBuildRunning     = errors.New("a build of this image is already queued or running")
	ErrBuildLogFinished = errors.New("the build log is already finished")
)

const (
	BuildQueued    = "queued"
	BuildRunning   = "running"
	BuildSucceeded = "succeeded"
	BuildFailed    = "failed"
	BuildCancelled = "cancelled"

	VersionFromBuild = "build"
	VersionFromPush  = "push"

	AutoDaily  = "daily"
	AutoWeekly = "weekly"

	buildLogKeep      = 5 << 20
	buildLogMarker    = "\n--- log truncated ---\n"
	BuildLogLiveCap   = 64 << 20
	buildLogCapMarker = "\n--- log limit reached, further output dropped ---\n"
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
	PushError      string
	CreatedBy      uuid.UUID
	CreatedAt      time.Time

	AutoSchedule     string // "", AutoDaily or AutoWeekly
	AutoOnBaseChange bool
	AutoBy           *uuid.UUID // automatic builds run as this user
	AutoRevision     *int       // the newest recipe revision approved for automatic builds
	AutoApprovedAt   *time.Time
	AutoError        string
	AutoLastAt       *time.Time
	AutoCheckedAt    *time.Time // last base-image check that resolved every base
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
	ImageID        uuid.UUID
	Digest         string
	PlatformDigest string // the linux/amd64 child when Digest is an index; nodes pull and cache it
	Source         string
	Revision       *int
	BuildID        *uuid.UUID
	SizeBytes      int64
	CreatedBy      *uuid.UUID
	CreatedAt      time.Time
	Layers         []ImageLayer
	BaseDigests    []BaseDigest
}

// BaseDigest is a FROM reference of a build and the linux/amd64 manifest digest it resolved to.
type BaseDigest struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

type ImageLayer struct {
	Digest string
	Size   int64
}

type ImageBuild struct {
	ID, ImageID           uuid.UUID
	Revision              int
	State                 string
	NodeID, VMID          *uuid.UUID
	StartedBy             uuid.UUID
	Automatic             bool
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

const imageCols = "id, coalesce(org_id, '00000000-0000-0000-0000-000000000000'::uuid), scope, coalesce(team_id, user_id), name, description, recipe_revision, platform, push_error, coalesce(created_by, '00000000-0000-0000-0000-000000000000'::uuid), created_at, " +
	"auto_schedule, auto_on_base_change, auto_by, auto_revision, auto_approved_at, auto_error, auto_last_at, auto_checked_at"

func scanImage(r pgx.Row) (v Image, err error) {
	err = r.Scan(&v.ID, &v.OrgID, &v.Scope, &v.OwnerID, &v.Name, &v.Description, &v.RecipeRevision, &v.Platform, &v.PushError, &v.CreatedBy, &v.CreatedAt,
		&v.AutoSchedule, &v.AutoOnBaseChange, &v.AutoBy, &v.AutoRevision, &v.AutoApprovedAt, &v.AutoError, &v.AutoLastAt, &v.AutoCheckedAt)
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

// Image returns an image of the org or the platform image.
func (s *Store) Image(ctx context.Context, org, id uuid.UUID) (Image, error) {
	return scanImage(s.q.QueryRow(ctx, "SELECT "+imageCols+" FROM images WHERE (org_id = $1 OR platform) AND id = $2", org, id))
}

// ImageByID finds an org image without knowing its org, for registry repositories that only carry the id.
func (s *Store) ImageByID(ctx context.Context, id uuid.UUID) (Image, error) {
	return scanImage(s.q.QueryRow(ctx, "SELECT "+imageCols+" FROM images WHERE id = $1 AND NOT platform", id))
}

// LockImage locks the org's image row for the rest of the transaction.
func (s *Store) LockImage(ctx context.Context, org, id uuid.UUID) (Image, error) {
	return scanImage(s.q.QueryRow(ctx, "SELECT "+imageCols+" FROM images WHERE org_id = $1 AND id = $2 FOR UPDATE", org, id))
}

// PlatformImage returns the one global platform image, creating it on first use.
func (s *Store) PlatformImage(ctx context.Context) (Image, error) {
	img, err := scanImage(s.q.QueryRow(ctx, "SELECT "+imageCols+" FROM images WHERE platform"))
	if !errors.Is(err, ErrNotFound) {
		return img, err
	}
	err = s.InTx(ctx, func(tx *Store) error {
		// No unique index can cover the NULL org, so concurrent first uses serialize here.
		if _, err := tx.q.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('observatory.platform-image'))"); err != nil {
			return err
		}
		img, err = scanImage(tx.q.QueryRow(ctx, `
			INSERT INTO images (scope, name, description, platform)
			SELECT 'org', 'standard', 'Standard image (ghcr.io/asterism-dev/observatory-guest), version chosen by the node', true
			WHERE NOT EXISTS (SELECT 1 FROM images WHERE platform) RETURNING `+imageCols))
		if errors.Is(err, ErrNotFound) {
			img, err = scanImage(tx.q.QueryRow(ctx, "SELECT "+imageCols+" FROM images WHERE platform"))
		}
		return err
	})
	return img, err
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

// SetImageAuto changes the settings and clears the error; by becomes the user automatic builds run as, keeping an approval only if it was by's.
func (s *Store) SetImageAuto(ctx context.Context, image uuid.UUID, schedule string, onBaseChange bool, by uuid.UUID) error {
	_, err := s.q.Exec(ctx, `UPDATE images SET auto_schedule = $2, auto_on_base_change = $3, auto_by = $4, auto_error = '',
		auto_revision = CASE WHEN auto_by IS NOT DISTINCT FROM $4 THEN auto_revision END,
		auto_approved_at = CASE WHEN auto_by IS NOT DISTINCT FROM $4 THEN auto_approved_at END WHERE id = $1`,
		image, schedule, onBaseChange, by)
	return err
}

// ApproveImageRevision lets automatic builds run revision as by; ErrConflict when it is no longer the current revision.
func (s *Store) ApproveImageRevision(ctx context.Context, image uuid.UUID, revision int, by uuid.UUID) error {
	tag, err := s.q.Exec(ctx, `UPDATE images SET auto_revision = $2, auto_by = $3, auto_approved_at = now(), auto_error = ''
		WHERE id = $1 AND recipe_revision = $2`, image, revision, by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

// RecipeRevisionAuthor returns who saved revision n of image.
func (s *Store) RecipeRevisionAuthor(ctx context.Context, image uuid.UUID, n int) (uuid.UUID, error) {
	var by uuid.UUID
	err := s.q.QueryRow(ctx, "SELECT created_by FROM recipe_revisions WHERE image_id = $1 AND number = $2", image, n).Scan(&by)
	if errors.Is(err, pgx.ErrNoRows) {
		return by, ErrNotFound
	}
	return by, err
}

// AutoBuildState sums up an image's builds for the automatic rebuild checks.
func (s *Store) AutoBuildState(ctx context.Context, image uuid.UUID) (active bool, lastAutomatic, lastSuccess *time.Time, err error) {
	err = s.q.QueryRow(ctx, `SELECT coalesce(bool_or(state IN ('queued', 'running')), false),
		max(queued_at) FILTER (WHERE automatic), max(finished_at) FILTER (WHERE state = 'succeeded')
		FROM image_builds WHERE image_id = $1`, image).Scan(&active, &lastAutomatic, &lastSuccess)
	return
}

// CreateFailedImageBuild records an automatic build that may not run as failed at once, so no scheduler ever sees it queued.
func (s *Store) CreateFailedImageBuild(ctx context.Context, b ImageBuild, msg string, at time.Time) (ImageBuild, error) {
	err := s.InTx(ctx, func(tx *Store) error {
		var err error
		if b, err = tx.CreateImageBuild(ctx, b); err != nil {
			return err
		}
		if b, err = tx.FinishImageBuild(ctx, b.ID, BuildFailed, "", msg, at); err != nil {
			return err
		}
		if err := tx.FinishBuildLog(ctx, b.ID); err != nil {
			return err
		}
		if b.Automatic {
			return tx.SetImageAutoResult(ctx, b.ImageID, msg, at)
		}
		return nil
	})
	return b, err
}

// AutoImages lists the org images with automatic rebuilds on and a recipe, grouped by org.
func (s *Store) AutoImages(ctx context.Context) ([]Image, error) {
	rows, err := s.q.Query(ctx, "SELECT "+imageCols+` FROM images
		WHERE NOT platform AND recipe_revision IS NOT NULL AND (auto_schedule <> '' OR auto_on_base_change) ORDER BY org_id, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Image, error) { return scanImage(r) })
}

// SetImageAutoResult records the outcome of an automatic rebuild; an empty msg clears the error.
func (s *Store) SetImageAutoResult(ctx context.Context, image uuid.UUID, msg string, at time.Time) error {
	_, err := s.q.Exec(ctx, "UPDATE images SET auto_error = $2, auto_last_at = $3 WHERE id = $1", image, msg, at)
	return err
}

// SetImageAutoCheckError shows a failed base-image check unless a failed build's error is showing; errors of checks start with checkPrefix.
func (s *Store) SetImageAutoCheckError(ctx context.Context, image uuid.UUID, msg, checkPrefix string) error {
	_, err := s.q.Exec(ctx, `UPDATE images SET auto_error = $2 WHERE id = $1 AND (auto_error = '' OR starts_with(auto_error, $3))`, image, msg, checkPrefix)
	return err
}

// SetImageAutoChecked records a base-image check that resolved every base and clears an error starting with checkPrefix.
func (s *Store) SetImageAutoChecked(ctx context.Context, image uuid.UUID, at time.Time, checkPrefix string) error {
	_, err := s.q.Exec(ctx, `UPDATE images SET auto_checked_at = $2,
		auto_error = CASE WHEN starts_with(auto_error, $3) THEN '' ELSE auto_error END WHERE id = $1`, image, at, checkPrefix)
	return err
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

// AddImageVersion records the version with its layers; an existing version stays as it is.
func (s *Store) AddImageVersion(ctx context.Context, v ImageVersion) error {
	digests, sizes := layerColumns(v.Layers)
	return s.InTx(ctx, func(tx *Store) error {
		if _, err := tx.q.Exec(ctx, `
			INSERT INTO image_versions (image_id, digest, source, revision, build_id, size_bytes, created_by, platform_digest) VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''))
			ON CONFLICT (image_id, digest) DO NOTHING`, v.ImageID, v.Digest, v.Source, v.Revision, v.BuildID, v.SizeBytes, v.CreatedBy, v.PlatformDigest); err != nil {
			return err
		}
		_, err := tx.q.Exec(ctx, `
			INSERT INTO image_version_layers (image_id, version_digest, digest, size) SELECT $1, $2, d, n FROM unnest($3::text[], $4::bigint[]) AS l(d, n)
			ON CONFLICT DO NOTHING`, v.ImageID, v.Digest, digests, sizes)
		return err
	})
}

func layerColumns(layers []ImageLayer) (digests []string, sizes []int64) {
	for _, l := range layers {
		digests, sizes = append(digests, l.Digest), append(sizes, l.Size)
	}
	return digests, sizes
}

const versionColumns = "image_id, digest, COALESCE(platform_digest, ''), source, revision, build_id, size_bytes, created_by, created_at, base_digests"

func scanVersion(r pgx.Row) (v ImageVersion, err error) {
	var bases []byte
	err = r.Scan(&v.ImageID, &v.Digest, &v.PlatformDigest, &v.Source, &v.Revision, &v.BuildID, &v.SizeBytes, &v.CreatedBy, &v.CreatedAt, &bases)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	} else if err != nil {
		return v, err
	}
	return v, json.Unmarshal(bases, &v.BaseDigests)
}

// SetVersionBaseDigests records the base images the build of a version resolved.
func (s *Store) SetVersionBaseDigests(ctx context.Context, image uuid.UUID, digest string, bases []BaseDigest) error {
	if bases == nil {
		bases = []BaseDigest{}
	}
	raw, err := json.Marshal(bases)
	if err != nil {
		return err
	}
	tag, err := s.q.Exec(ctx, "UPDATE image_versions SET base_digests = $3 WHERE image_id = $1 AND digest = $2", image, digest, raw)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ImageWithVersion returns the version digest of an image of org, the oldest first, else ErrNotFound.
func (s *Store) ImageWithVersion(ctx context.Context, org uuid.UUID, digest string) (ImageVersion, error) {
	return scanVersion(s.q.QueryRow(ctx, `
		SELECT `+versionColumns+` FROM image_versions
		WHERE image_id IN (SELECT id FROM images WHERE org_id = $1) AND digest = $2 ORDER BY created_at LIMIT 1`, org, digest))
}

// SetImagePushError shows why the last push did not become a version; empty clears it.
func (s *Store) SetImagePushError(ctx context.Context, image uuid.UUID, msg string) error {
	_, err := s.q.Exec(ctx, "UPDATE images SET push_error = $2 WHERE id = $1", image, msg)
	return err
}

func (s *Store) ImageVersions(ctx context.Context, image uuid.UUID) ([]ImageVersion, error) {
	rows, err := s.q.Query(ctx, `SELECT `+versionColumns+` FROM image_versions WHERE image_id = $1 ORDER BY created_at DESC, digest`, image)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ImageVersion, error) { return scanVersion(r) })
}

func (s *Store) ImageVersion(ctx context.Context, image uuid.UUID, digest string) (ImageVersion, error) {
	return scanVersion(s.q.QueryRow(ctx, `SELECT `+versionColumns+` FROM image_versions WHERE image_id = $1 AND digest = $2`, image, digest))
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

func (s *Store) DeleteImageTag(ctx context.Context, image uuid.UUID, tag string) error {
	res, err := s.q.Exec(ctx, "DELETE FROM image_tags WHERE image_id = $1 AND tag = $2", image, tag)
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

const buildCols = "id, image_id, revision, state, node_id, vm_id, started_by, automatic, queued_at, started_at, finished_at, digest, error"

func scanBuild(r pgx.Row) (b ImageBuild, err error) {
	err = r.Scan(&b.ID, &b.ImageID, &b.Revision, &b.State, &b.NodeID, &b.VMID, &b.StartedBy, &b.Automatic, &b.QueuedAt, &b.StartedAt, &b.FinishedAt, &b.Digest, &b.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return b, err
}

func (s *Store) CreateImageBuild(ctx context.Context, b ImageBuild) (ImageBuild, error) {
	b, err := scanBuild(s.q.QueryRow(ctx, `
		INSERT INTO image_builds (image_id, revision, started_by, automatic) VALUES ($1, $2, $3, $4) RETURNING `+buildCols, b.ImageID, b.Revision, b.StartedBy, b.Automatic))
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

// ActiveBuilds lists queued and running builds of every org, oldest first.
func (s *Store) ActiveBuilds(ctx context.Context) ([]ImageBuild, error) {
	rows, err := s.q.Query(ctx, "SELECT "+buildCols+" FROM image_builds WHERE state IN ('queued', 'running') ORDER BY queued_at, id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ImageBuild, error) { return scanBuild(r) })
}

// StartImageBuild moves a queued build to running on its builder VM; ErrConflict when it is no longer queued.
func (s *Store) StartImageBuild(ctx context.Context, id, node, vm uuid.UUID, secret string, at time.Time) error {
	hash := sha256.Sum256([]byte(secret))
	tag, err := s.q.Exec(ctx, `UPDATE image_builds SET state = 'running', node_id = $2, vm_id = $3, secret_hash = $4, started_at = $5
		WHERE id = $1 AND state = 'queued'`, id, node, vm, hash[:], at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

// FinishImageBuild ends a queued or running build; ErrConflict when it already ended.
func (s *Store) FinishImageBuild(ctx context.Context, id uuid.UUID, state, digest, msg string, at time.Time) (ImageBuild, error) {
	b, err := scanBuild(s.q.QueryRow(ctx, `UPDATE image_builds SET state = $2, digest = $3, error = $4, finished_at = $5, secret_hash = NULL
		WHERE id = $1 AND state IN ('queued', 'running') RETURNING `+buildCols, id, state, digest, msg, at))
	if errors.Is(err, ErrNotFound) {
		if _, err := s.ImageBuild(ctx, id); err != nil {
			return ImageBuild{}, err
		}
		return ImageBuild{}, ErrConflict
	}
	return b, err
}

// BuildBySecret returns the running build the secret belongs to, else ErrNotFound.
func (s *Store) BuildBySecret(ctx context.Context, secret string) (ImageBuild, error) {
	hash := sha256.Sum256([]byte(secret))
	return scanBuild(s.q.QueryRow(ctx, "SELECT "+buildCols+" FROM image_builds WHERE secret_hash = $1 AND state = 'running'", hash[:]))
}

// RunningBuildOnVM returns the running build of a builder VM, else ErrNotFound.
func (s *Store) RunningBuildOnVM(ctx context.Context, vm uuid.UUID) (ImageBuild, error) {
	return scanBuild(s.q.QueryRow(ctx, "SELECT "+buildCols+" FROM image_builds WHERE vm_id = $1 AND state = 'running'", vm))
}

// BuildSecret derives the build's credential from the org key, so the hub can render the builder VM's env again while only a hash is stored.
func (s *Store) BuildSecret(ctx context.Context, org, build uuid.UUID) (string, error) {
	key, err := s.orgKey(ctx, org)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte("image-build:" + build.String()))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *Store) ImageBuilds(ctx context.Context, image uuid.UUID) ([]ImageBuild, error) {
	rows, err := s.q.Query(ctx, "SELECT "+buildCols+" FROM image_builds WHERE image_id = $1 ORDER BY queued_at DESC, id", image)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ImageBuild, error) { return scanBuild(r) })
}

// AppendBuildLog stores chunk up to BuildLogLiveCap, then drops further output after one marker line.
// It returns how many bytes of chunk were stored, not counting the marker.
func (s *Store) AppendBuildLog(ctx context.Context, build uuid.UUID, chunk []byte) (accepted int, err error) {
	err = s.InTx(ctx, func(tx *Store) error {
		// FOR UPDATE serializes appends, so chunk ids also commit in order for BuildLogSince.
		finished, err := tx.lockBuildLog(ctx, build, "FOR UPDATE")
		if err != nil {
			return err
		}
		if finished {
			return ErrBuildLogFinished
		}
		var stored int64
		if err := tx.q.QueryRow(ctx, "SELECT live_log_bytes FROM image_builds WHERE id = $1", build).Scan(&stored); err != nil {
			return err
		}
		// Above the cap means the marker is already stored.
		if stored > BuildLogLiveCap {
			return nil
		}
		accepted = len(chunk)
		if room := BuildLogLiveCap - stored; int64(len(chunk)) > room {
			accepted = int(room)
			chunk = append(chunk[:room:room], buildLogCapMarker...)
			stored = BuildLogLiveCap + 1
		} else {
			stored += int64(len(chunk))
		}
		if _, err := tx.q.Exec(ctx, "UPDATE image_builds SET live_log_bytes = $2 WHERE id = $1", build, stored); err != nil {
			return err
		}
		_, err = tx.q.Exec(ctx, "INSERT INTO image_build_log_chunks (build_id, chunk) VALUES ($1, $2)", build, chunk)
		return err
	})
	if err != nil {
		accepted = 0
	}
	return accepted, err
}

// BuildLogSince returns the chunks after chunk id after and the last id read, or finished once the log is compacted.
func (s *Store) BuildLogSince(ctx context.Context, build uuid.UUID, after int64) (chunk []byte, last int64, finished bool, err error) {
	if finished, err = s.lockBuildLog(ctx, build, ""); err != nil || finished {
		return nil, after, finished, err
	}
	err = s.q.QueryRow(ctx, `SELECT coalesce(string_agg(chunk, ''::bytea ORDER BY id), ''::bytea), coalesce(max(id), $2)
		FROM image_build_log_chunks WHERE build_id = $1 AND id > $2`, build, after).Scan(&chunk, &last)
	return chunk, last, false, err
}

// lockBuildLog locks the build row so appends and the compaction in FinishBuildLog cannot interleave.
func (s *Store) lockBuildLog(ctx context.Context, build uuid.UUID, mode string) (finished bool, err error) {
	err = s.q.QueryRow(ctx, "SELECT log IS NOT NULL FROM image_builds WHERE id = $1 "+mode, build).Scan(&finished)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return finished, err
}

// FinishBuildLog replaces the chunks with one gzip blob, cutting the middle above 2*buildLogKeep.
func (s *Store) FinishBuildLog(ctx context.Context, build uuid.UUID) error {
	return s.InTx(ctx, func(tx *Store) error {
		if finished, err := tx.lockBuildLog(ctx, build, "FOR UPDATE"); err != nil || finished {
			return err
		}
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

// OrgImageUsage sums each layer digest once however many versions share it, counting extra layers as if recorded.
func (s *Store) OrgImageUsage(ctx context.Context, org uuid.UUID, extra ...ImageLayer) (int64, error) {
	digests, sizes := layerColumns(extra)
	var n int64
	err := s.q.QueryRow(ctx, `
		SELECT coalesce(sum(size), 0)::bigint FROM (
			SELECT DISTINCT ON (digest) size FROM (
				SELECT l.digest, l.size FROM image_version_layers l JOIN images i ON i.id = l.image_id WHERE i.org_id = $1
				UNION ALL SELECT * FROM unnest($2::text[], $3::bigint[])) a) d`, org, digests, sizes).Scan(&n)
	return n, err
}
