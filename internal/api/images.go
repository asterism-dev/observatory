package api

import (
	"archive/tar"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/iaminternal"
	"github.com/asterism-dev/observatory/internal/images"
	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/platform/textx"
	"github.com/asterism-dev/observatory/internal/store"
)

// ManifestDeleter removes a manifest from the registry; without one, deleting versions only drops their rows.
type ManifestDeleter interface {
	DeleteManifest(ctx context.Context, repo, digest string) error
}

const (
	maxRecipeBytes = 50 << 20
	maxRecipeFiles = 1000
	maxRecipeText  = 1 << 20
	platformRef    = "observatory/standard"
	latestTag      = "latest"
)

var tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// httpError aborts a commit with a status and message of its own.
type httpError struct {
	status int
	msg    string
}

func (e httpError) Error() string { return e.msg }

// codedError adds a machine-readable code clients act on without matching the message.
type codedError struct {
	httpError
	code string
}

const (
	codeWidenDeletesVersions = "widen_deletes_versions"
	codeRecipeChanged        = "recipe_changed"
)

var errRecipeChanged = httpError{http.StatusConflict, "the recipe changed since your base revision, reload it"}

func imageObject(scope store.EnvScope, owner *uuid.UUID) string {
	switch scope {
	case store.EnvScopeTeam:
		return authz.TeamImagesObject(*owner)
	case store.EnvScopeUser:
		return authz.UserImagesObject(*owner)
	}
	return authz.ObjImages
}

func imageRepo(id uuid.UUID) string { return "i/" + id.String() }

func sameOwner(a, b *uuid.UUID) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func scopeRank(s store.EnvScope) int {
	switch s {
	case store.EnvScopeUser:
		return 0
	case store.EnvScopeTeam:
		return 1
	}
	return 2
}

func imageAudit(r *http.Request, action string, img store.Image) store.AuditEntry {
	e := auditEntry(r, &img.OrgID, action, imageObject(img.Scope, img.OwnerID), store.ResultAllowed)
	e.Details = map[string]any{"imageId": img.ID, "name": img.Name, "scope": img.Scope, "ownerId": img.OwnerID}
	return e
}

func validDescription(s string) bool {
	return utf8.RuneCountInString(s) <= 1000 && textx.Sanitize(s) == s
}

// imageFromPath loads {id}; another member's private image is 404, and the platform image only allows reads.
func (h *api) imageFromPath(w http.ResponseWriter, r *http.Request, act string) (store.Image, bool) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return store.Image{}, false
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return store.Image{}, false
	}
	img, err := h.store.Image(r.Context(), m.OrgID, id)
	p, _ := auth.PrincipalFrom(r.Context())
	if errors.Is(err, store.ErrNotFound) || (err == nil && img.Scope == store.EnvScopeUser && *img.OwnerID != p.User.ID) {
		writeError(w, http.StatusNotFound, "not found")
		return store.Image{}, false
	} else if err != nil {
		serverError(w, "image", err)
		return store.Image{}, false
	}
	if img.Platform {
		if act != "artifacts.images.list" {
			writeError(w, http.StatusForbidden, "the platform image is read-only")
			return store.Image{}, false
		}
		return img, true
	}
	return img, h.authorize(w, r, m.OrgID, imageObject(img.Scope, img.OwnerID), act)
}

func (h *api) teamNames(ctx context.Context, org uuid.UUID) (map[uuid.UUID]string, error) {
	teams, err := iaminternal.ListTeams(ctx, h.iam, org)
	names := make(map[uuid.UUID]string, len(teams))
	for _, t := range teams {
		names[t.ID] = t.Name
	}
	return names, err
}

// imageRef is the qualified reference users type for img, with tag or digest when given.
func imageRef(img store.Image, teams map[uuid.UUID]string, tag, digest string) string {
	if img.Platform {
		return platformRef
	}
	q := "org"
	switch img.Scope {
	case store.EnvScopeTeam:
		q = teams[*img.OwnerID]
	case store.EnvScopeUser:
		q = "me"
	}
	ref := q + "/" + img.Name
	if digest != "" {
		return ref + "@" + digest
	}
	if tag != "" {
		return ref + ":" + tag
	}
	return ref
}

type imageJSON struct {
	ID             uuid.UUID         `json:"id"`
	Name           string            `json:"name"`
	Description    string            `json:"description"`
	Ref            string            `json:"ref"`
	Repository     string            `json:"repository,omitempty"`
	Scope          store.EnvScope    `json:"scope"`
	OwnerID        *uuid.UUID        `json:"ownerId,omitempty"`
	OwnerName      string            `json:"ownerName,omitempty"`
	Platform       bool              `json:"platform"`
	RecipeRevision *int              `json:"recipeRevision"`
	Tags           map[string]string `json:"tags"`
	LatestDigest   string            `json:"latestDigest"`
	SizeBytes      int64             `json:"sizeBytes"`
	LastBuildState string            `json:"lastBuildState"`
	PushError      string            `json:"pushError"`
	CreatedAt      time.Time         `json:"createdAt"`
	CanEdit        bool              `json:"canEdit"`
	CanBuild       bool              `json:"canBuild"`

	AutoSchedule     string     `json:"autoSchedule"`
	AutoOnBaseChange bool       `json:"autoOnBaseChange"`
	AutoError        string     `json:"autoError"`
	AutoLastAt       *time.Time `json:"autoLastAt"`
	AutoRevision     *int       `json:"autoRevision"`
	// AutoNeedsApproval: the current revision is by someone else than the user automatic builds run as and not approved yet.
	AutoNeedsApproval bool `json:"autoNeedsApproval"`
}

// ponytail: three queries per image; join them in the store if image lists get long.
func (h *api) imageJSON(ctx context.Context, img store.Image, teams map[uuid.UUID]string) (imageJSON, error) {
	j := imageJSON{ID: img.ID, Name: img.Name, Description: img.Description, Ref: imageRef(img, teams, "", ""), Scope: img.Scope,
		Platform: img.Platform, RecipeRevision: img.RecipeRevision, Tags: map[string]string{}, PushError: img.PushError, CreatedAt: img.CreatedAt,
		AutoSchedule: img.AutoSchedule, AutoOnBaseChange: img.AutoOnBaseChange, AutoError: img.AutoError, AutoLastAt: img.AutoLastAt,
		AutoRevision: img.AutoRevision}
	if img.Platform {
		return j, nil
	}
	if img.RecipeRevision != nil && (img.AutoSchedule != "" || img.AutoOnBaseChange) {
		author, err := h.store.RecipeRevisionAuthor(ctx, img.ID, *img.RecipeRevision)
		if err != nil {
			return j, err
		}
		j.AutoNeedsApproval = needsApproval(img, author)
	}
	p, _ := auth.PrincipalFrom(ctx)
	sub, obj := authz.UserSubject(p.User.ID), imageObject(img.Scope, img.OwnerID)
	j.Repository, j.OwnerID = imageRepo(img.ID), img.OwnerID
	j.CanEdit, j.CanBuild = h.z.Allowed(sub, img.OrgID, obj, "artifacts.images.update"), h.z.Allowed(sub, img.OrgID, obj, "build.builds.create")
	switch img.Scope {
	case store.EnvScopeTeam:
		j.OwnerName = teams[*img.OwnerID]
	case store.EnvScopeUser:
		j.OwnerName = p.User.Name
	}
	var err error
	if j.Tags, err = h.store.ImageTags(ctx, img.ID); err != nil {
		return j, err
	}
	if j.LatestDigest = j.Tags[latestTag]; j.LatestDigest != "" {
		v, err := h.store.ImageVersion(ctx, img.ID, j.LatestDigest)
		if err != nil {
			return j, err
		}
		j.SizeBytes = v.SizeBytes
	}
	builds, err := h.store.ImageBuilds(ctx, img.ID)
	if len(builds) > 0 {
		j.LastBuildState = builds[0].State
	}
	return j, err
}

func (h *api) listImages(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	platform, err := h.store.PlatformImage(ctx)
	if err != nil {
		serverError(w, "platform image", err)
		return
	}
	list, err := h.store.ListImages(ctx, m.OrgID)
	if err != nil {
		serverError(w, "images", err)
		return
	}
	teams, err := h.teamNames(ctx, m.OrgID)
	if err != nil {
		serverError(w, "teams", err)
		return
	}
	p, _ := auth.PrincipalFrom(ctx)
	sub := authz.UserSubject(p.User.ID)
	out := []imageJSON{}
	for _, img := range append([]store.Image{platform}, list...) {
		// Another member's private images are never listed, whatever rules a custom role grants.
		if !img.Platform && ((img.Scope == store.EnvScopeUser && *img.OwnerID != p.User.ID) ||
			!h.z.Allowed(sub, m.OrgID, imageObject(img.Scope, img.OwnerID), "artifacts.images.list")) {
			continue
		}
		j, err := h.imageJSON(ctx, img, teams)
		if err != nil {
			serverError(w, "image", err)
			return
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) createImage(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	var body struct {
		Name, Description string
		Scope             store.EnvScope
		TeamID            *uuid.UUID
	}
	if !readJSON(w, r, &body) {
		return
	}
	if !vmNamePattern.MatchString(body.Name) {
		writeError(w, http.StatusBadRequest, "invalid name")
		return
	}
	if !validDescription(body.Description) {
		writeError(w, http.StatusBadRequest, "invalid description")
		return
	}
	owner, ok := h.envTarget(w, r, m.OrgID, body.Scope, body.TeamID)
	if !ok || !h.authorize(w, r, m.OrgID, imageObject(body.Scope, owner), "artifacts.images.update") {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	img := store.Image{OrgID: m.OrgID, Scope: body.Scope, OwnerID: owner, Name: body.Name, Description: body.Description, CreatedBy: p.User.ID}
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if img, err = tx.CreateImage(r.Context(), img); err != nil {
			return err
		}
		return tx.Audit(r.Context(), imageAudit(r, store.ActionImageCreate, img))
	}) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": img.ID})
}

func (h *api) writeImage(w http.ResponseWriter, r *http.Request, img store.Image) {
	teams, err := h.teamNames(r.Context(), img.OrgID)
	if err != nil {
		serverError(w, "teams", err)
		return
	}
	j, err := h.imageJSON(r.Context(), img, teams)
	if err != nil {
		serverError(w, "image", err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (h *api) getImage(w http.ResponseWriter, r *http.Request) {
	if img, ok := h.imageFromPath(w, r, "artifacts.images.list"); ok {
		h.writeImage(w, r, img)
	}
}

// refAllowed applies the scope rule: an image may only use references at its own level or broader.
func refAllowed(ref store.RecipeRef, scope store.EnvScope, owner *uuid.UUID) bool {
	switch ref.Scope {
	case store.EnvScopeOrg:
		return true
	case store.EnvScopeTeam:
		return scope == store.EnvScopeUser || (scope == store.EnvScopeTeam && sameOwner(ref.OwnerID, owner))
	}
	return scope == store.EnvScopeUser && sameOwner(ref.OwnerID, owner)
}

func describeRef(ref store.RecipeRef) string {
	return ref.Kind + " " + string(ref.Scope) + "/" + ref.Name
}

// memberNames maps org's members, as the policy has them, to their display names.
func (h *api) memberNames(ctx context.Context, org uuid.UUID) (map[uuid.UUID]string, error) {
	return iaminternal.UserNames(ctx, h.iam, h.z.Enforcer().UsersOf(org))
}

// defaultSees reports whether a default at scope/owner may point at img.
func defaultSees(e *authz.Enforcer, scope store.EnvScope, owner *uuid.UUID, img store.Image) bool {
	if img.Platform || img.Scope == store.EnvScopeOrg {
		return true
	}
	switch {
	case scope == store.EnvScopeTeam || img.Scope == store.EnvScopeUser:
		return scope == img.Scope && sameOwner(owner, img.OwnerID)
	case scope == store.EnvScopeUser:
		return slices.Contains(e.TeamMembers(img.OrgID, *img.OwnerID), *owner)
	}
	return false
}

// dropBlindDefaults clears defaults that can no longer see img after a visibility change.
func dropBlindDefaults(ctx context.Context, tx *store.Store, e *authz.Enforcer, img store.Image) error {
	ds, err := tx.ImageDefaults(ctx, img.OrgID)
	if err != nil {
		return err
	}
	for _, d := range ds {
		if d.ImageID != img.ID {
			continue
		}
		if !defaultSees(e, d.Scope, d.OwnerID, img) {
			if err := tx.ClearImageDefault(ctx, d.OrgID, d.Scope, d.OwnerID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *api) updateImage(w http.ResponseWriter, r *http.Request) {
	old, ok := h.imageFromPath(w, r, "artifacts.images.update")
	if !ok {
		return
	}
	ctx := r.Context()
	var body struct {
		Name, Description     *string
		Scope                 *store.EnvScope
		TeamID                *uuid.UUID
		ConfirmDeleteVersions bool
		AutoSchedule          *string
		AutoOnBaseChange      *bool
		ApproveRevision       *int
	}
	if !readJSON(w, r, &body) {
		return
	}
	v := old
	auto := body.AutoSchedule != nil || body.AutoOnBaseChange != nil
	if auto || body.ApproveRevision != nil {
		if body.AutoSchedule != nil {
			if !slices.Contains([]string{"", store.AutoDaily, store.AutoWeekly}, *body.AutoSchedule) {
				writeError(w, http.StatusBadRequest, "invalid autoSchedule")
				return
			}
			v.AutoSchedule = *body.AutoSchedule
		}
		if body.AutoOnBaseChange != nil {
			v.AutoOnBaseChange = *body.AutoOnBaseChange
		}
		// Automatic builds run as whoever set them up, so that person needs the build right too.
		if !h.authorize(w, r, old.OrgID, imageObject(old.Scope, old.OwnerID), "build.builds.create") {
			return
		}
		if old.RecipeRevision == nil && (v.AutoSchedule != "" || v.AutoOnBaseChange) {
			writeError(w, http.StatusUnprocessableEntity, "the image has no recipe")
			return
		}
	}
	if body.Name != nil {
		if !vmNamePattern.MatchString(*body.Name) {
			writeError(w, http.StatusBadRequest, "invalid name")
			return
		}
		v.Name = *body.Name
	}
	if body.Description != nil {
		if !validDescription(*body.Description) {
			writeError(w, http.StatusBadRequest, "invalid description")
			return
		}
		v.Description = *body.Description
	}
	widening := false
	if body.Scope != nil {
		owner, ok := h.envTarget(w, r, old.OrgID, *body.Scope, body.TeamID)
		if !ok {
			return
		}
		v.Scope, v.OwnerID = *body.Scope, owner
		if v.Scope != old.Scope || !sameOwner(v.OwnerID, old.OwnerID) {
			if !h.authorize(w, r, old.OrgID, imageObject(v.Scope, v.OwnerID), "artifacts.images.update") {
				return
			}
			// Moving to another team shares the versions with new people just like widening.
			widening = scopeRank(v.Scope) >= scopeRank(old.Scope)
			if old.RecipeRevision != nil {
				rev, err := h.store.RecipeRevision(ctx, old.ID, *old.RecipeRevision)
				if err != nil {
					serverError(w, "recipe revision", err)
					return
				}
				var bad []string
				for _, ref := range rev.Refs {
					if !refAllowed(ref, v.Scope, v.OwnerID) {
						bad = append(bad, describeRef(ref))
					}
				}
				if len(bad) > 0 {
					writeError(w, http.StatusUnprocessableEntity, "the recipe references scopes narrower than the new visibility: "+strings.Join(bad, ", "))
					return
				}
			}
		}
	}
	var deleted []string
	if !h.commit(w, r, func(tx *store.Store) error {
		if widening {
			vs, err := tx.ImageVersions(ctx, old.ID)
			if err != nil {
				return err
			}
			if len(vs) > 0 && !body.ConfirmDeleteVersions {
				return codedError{httpError{http.StatusConflict, fmt.Sprintf("widening visibility deletes %d versions", len(vs))}, codeWidenDeletesVersions}
			}
			if err := tx.DeleteImageVersions(ctx, old.ID); err != nil {
				return err
			}
			for _, ver := range vs {
				deleted = append(deleted, ver.Digest)
			}
		}
		p, _ := auth.PrincipalFrom(ctx)
		if auto {
			if err := tx.SetImageAuto(ctx, old.ID, v.AutoSchedule, v.AutoOnBaseChange, p.User.ID); err != nil {
				return err
			}
		}
		// Only the revision the client showed is approved, never one saved since.
		if body.ApproveRevision != nil {
			if err := tx.ApproveImageRevision(ctx, old.ID, *body.ApproveRevision, p.User.ID); errors.Is(err, store.ErrConflict) {
				return codedError{httpError{http.StatusConflict, errRecipeChanged.msg}, codeRecipeChanged}
			} else if err != nil {
				return err
			}
		}
		var err error
		if v, err = tx.UpdateImage(ctx, v); err != nil {
			return err
		}
		if err := dropBlindDefaults(ctx, tx, h.z.Enforcer(), v); err != nil {
			return err
		}
		e := imageAudit(r, store.ActionImageUpdate, v)
		e.Details["previousName"], e.Details["previousScope"], e.Details["previousOwnerId"] = old.Name, old.Scope, old.OwnerID
		e.Details["deletedVersions"] = len(deleted)
		if auto {
			e.Details["autoSchedule"], e.Details["autoOnBaseChange"] = v.AutoSchedule, v.AutoOnBaseChange
		}
		if body.ApproveRevision != nil {
			e.Details["approvedRevision"] = *body.ApproveRevision
		}
		return tx.Audit(ctx, e)
	}) {
		return
	}
	h.deleteManifests(ctx, v.ID, deleted)
	h.writeImage(w, r, v)
}

func (h *api) deleteImage(w http.ResponseWriter, r *http.Request) {
	img, ok := h.imageFromPath(w, r, "artifacts.images.update")
	if !ok {
		return
	}
	var digests []string
	if !h.commit(w, r, func(tx *store.Store) error {
		vs, err := tx.ImageVersions(r.Context(), img.ID)
		if err != nil {
			return err
		}
		for _, v := range vs {
			digests = append(digests, v.Digest)
		}
		// Defaults, tags and builds go with the image; VMs only keep their digest and are untouched.
		if err := tx.DeleteImage(r.Context(), img.OrgID, img.ID); err != nil {
			return err
		}
		return tx.Audit(r.Context(), imageAudit(r, store.ActionImageDelete, img))
	}) {
		return
	}
	h.deleteManifests(r.Context(), img.ID, digests)
	w.WriteHeader(http.StatusNoContent)
}

// deleteManifests runs after the rows are gone; a failure only leaves an unreachable manifest behind.
func (h *api) deleteManifests(ctx context.Context, image uuid.UUID, digests []string) {
	DeleteManifests(ctx, h.manifests, image, digests)
}

func DeleteManifests(ctx context.Context, m ManifestDeleter, image uuid.UUID, digests []string) {
	if m == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	for _, d := range digests {
		if err := m.DeleteManifest(ctx, imageRepo(image), d); err != nil {
			slog.Warn("delete manifest", "image", image, "digest", d, "err", err)
		}
	}
}

// OwnedImageVersions lists the version digests per image owned by a team or user, before their rows go.
func OwnedImageVersions(ctx context.Context, s *store.Store, org uuid.UUID, scope store.EnvScope, owner uuid.UUID) (map[uuid.UUID][]string, error) {
	list, err := s.ListImages(ctx, org)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID][]string{}
	for _, img := range list {
		if img.Scope != scope || *img.OwnerID != owner {
			continue
		}
		vs, err := s.ImageVersions(ctx, img.ID)
		if err != nil {
			return nil, err
		}
		for _, v := range vs {
			out[img.ID] = append(out[img.ID], v.Digest)
		}
	}
	return out, nil
}

type recipeRefJSON struct {
	Kind   string         `json:"kind"`
	Scope  store.EnvScope `json:"scope"`
	TeamID *uuid.UUID     `json:"teamId,omitempty"`
	Name   string         `json:"name"`
}

func toRefJSON(refs []store.RecipeRef) []recipeRefJSON {
	out := []recipeRefJSON{}
	for _, ref := range refs {
		j := recipeRefJSON{Kind: ref.Kind, Scope: ref.Scope, Name: ref.Name}
		if ref.Scope == store.EnvScopeTeam {
			j.TeamID = ref.OwnerID
		}
		out = append(out, j)
	}
	return out
}

// checkRefs validates requested references for img: scope rule, the caller's use right, and existence.
func (h *api) checkRefs(w http.ResponseWriter, r *http.Request, img store.Image, in []recipeRefJSON) ([]store.RecipeRef, bool) {
	ctx := r.Context()
	p, _ := auth.PrincipalFrom(ctx)
	var envs []store.EnvVar
	var igs []store.Integration
	out := []store.RecipeRef{}
	for _, j := range in {
		ref := store.RecipeRef{Kind: j.Kind, Scope: j.Scope, Name: j.Name}
		switch j.Scope {
		case store.EnvScopeTeam:
			ref.OwnerID = j.TeamID
		case store.EnvScopeUser:
			ref.OwnerID = &p.User.ID
		}
		validScope := j.Scope == store.EnvScopeOrg || j.Scope == store.EnvScopeUser || (j.Scope == store.EnvScopeTeam && j.TeamID != nil)
		if (j.Kind != "secret" && j.Kind != "integration") || !validScope || j.Name == "" {
			writeError(w, http.StatusUnprocessableEntity, "invalid reference")
			return nil, false
		}
		if j.Kind == "integration" && j.Name == string(integrations.GCloud) {
			writeError(w, http.StatusUnprocessableEntity, "gcloud cannot be used in builds")
			return nil, false
		}
		if !refAllowed(ref, img.Scope, img.OwnerID) {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("reference %s is not allowed for a %s image", describeRef(ref), img.Scope))
			return nil, false
		}
		// The build sees a reference by name only, so two scopes of one name would collide.
		if slices.ContainsFunc(out, func(o store.RecipeRef) bool {
			return o.Kind == ref.Kind && o.Name == ref.Name && (o.Scope != ref.Scope || !sameOwner(o.OwnerID, ref.OwnerID))
		}) {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("the recipe references two %ss named %s; keep one", ref.Kind, ref.Name))
			return nil, false
		}
		var found bool
		var err error
		if j.Kind == "secret" {
			if !h.authorize(w, r, img.OrgID, envObject(ref.Scope, ref.OwnerID), "secrets.variables.use") {
				return nil, false
			}
			if envs == nil {
				envs, err = h.store.EnvVars(ctx, img.OrgID)
			}
			found = slices.ContainsFunc(envs, func(v store.EnvVar) bool {
				return v.Scope == ref.Scope && sameOwner(v.OwnerID, ref.OwnerID) && v.Name == ref.Name
			})
		} else {
			if !h.authorize(w, r, img.OrgID, integrationObject(ref.Scope, ref.OwnerID), "secrets.integrations.use") {
				return nil, false
			}
			if igs == nil {
				igs, err = h.store.Integrations(ctx, img.OrgID)
			}
			found = slices.ContainsFunc(igs, func(v store.Integration) bool {
				return v.Scope == ref.Scope && sameOwner(v.OwnerID, ref.OwnerID) && string(v.Kind) == ref.Name
			})
		}
		if err != nil {
			serverError(w, "recipe references", err)
			return nil, false
		}
		if !found {
			writeError(w, http.StatusUnprocessableEntity, "unknown reference "+describeRef(ref))
			return nil, false
		}
		out = append(out, ref)
	}
	return out, true
}

func currentRevision(img store.Image) int {
	if img.RecipeRevision == nil {
		return 0
	}
	return *img.RecipeRevision
}

func validRecipePath(p string) bool {
	return p != "" && p != "." && p != ".." && !strings.HasPrefix(p, "../") && !path.IsAbs(p) && path.Clean(p) == p &&
		utf8.ValidString(p) && textx.Sanitize(p) == p
}

func checkRecipe(files []store.RecipeFile) *httpError {
	total := 0
	seen := map[string]bool{}
	for _, f := range files {
		total += len(f.Content)
		switch {
		case !validRecipePath(f.Path):
			return &httpError{http.StatusUnprocessableEntity, fmt.Sprintf("invalid path %q", f.Path)}
		case f.Mode > 0o777:
			return &httpError{http.StatusUnprocessableEntity, fmt.Sprintf("invalid mode for %q", f.Path)}
		case seen[f.Path]:
			return &httpError{http.StatusUnprocessableEntity, fmt.Sprintf("duplicate path %q", f.Path)}
		}
		seen[f.Path] = true
	}
	for _, f := range files {
		for dir := path.Dir(f.Path); dir != "."; dir = path.Dir(dir) {
			if seen[dir] {
				return &httpError{http.StatusUnprocessableEntity, fmt.Sprintf("%q is a file, so %q cannot be in it", dir, f.Path)}
			}
		}
	}
	switch {
	case len(files) > maxRecipeFiles:
		return &httpError{http.StatusRequestEntityTooLarge, fmt.Sprintf("a recipe holds at most %d files", maxRecipeFiles)}
	case total > maxRecipeBytes:
		return &httpError{http.StatusRequestEntityTooLarge, "a recipe holds at most 50 MB"}
	case !seen["Dockerfile"]:
		return &httpError{http.StatusUnprocessableEntity, "the recipe needs a Dockerfile at its root"}
	}
	return nil
}

func bodyError(err error, what string) *httpError {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return &httpError{http.StatusRequestEntityTooLarge, "a recipe holds at most 50 MB"}
	}
	return &httpError{http.StatusBadRequest, "invalid " + what}
}

// readRecipeTar reads a gzipped tar of regular files and directories, stopping as soon as a limit is crossed.
func readRecipeTar(rd io.Reader) ([]store.RecipeFile, *httpError) {
	zr, err := gzip.NewReader(rd)
	if err != nil {
		return nil, bodyError(err, "gzip")
	}
	tr := tar.NewReader(zr)
	var files []store.RecipeFile
	total := int64(0)
	for entries := 0; ; entries++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, bodyError(err, "tar")
		}
		if entries > 2*maxRecipeFiles || len(files) > maxRecipeFiles {
			return nil, &httpError{http.StatusRequestEntityTooLarge, fmt.Sprintf("a recipe holds at most %d files", maxRecipeFiles)}
		}
		switch hdr.Typeflag {
		case tar.TypeDir, tar.TypeXGlobalHeader:
			continue
		case tar.TypeReg:
		default:
			return nil, &httpError{http.StatusUnprocessableEntity, fmt.Sprintf("%q is not a regular file", hdr.Name)}
		}
		if total += hdr.Size; total > maxRecipeBytes {
			return nil, &httpError{http.StatusRequestEntityTooLarge, "a recipe holds at most 50 MB"}
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, bodyError(err, "tar")
		}
		files = append(files, store.RecipeFile{Path: strings.TrimPrefix(hdr.Name, "./"), Mode: uint32(hdr.Mode) & 0o777, Content: content})
	}
}

func (h *api) putRecipe(w http.ResponseWriter, r *http.Request) {
	img, ok := h.imageFromPath(w, r, "artifacts.images.update")
	if !ok {
		return
	}
	var in []recipeRefJSON
	if raw := r.Header.Get("X-Observatory-Refs"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid X-Observatory-Refs")
			return
		}
	}
	refs, ok := h.checkRefs(w, r, img, in)
	if !ok {
		return
	}
	// Tar headers and gzip framing add a little on top of the content limit.
	files, he := readRecipeTar(http.MaxBytesReader(w, r.Body, maxRecipeBytes+4<<20))
	if he != nil {
		writeError(w, he.status, he.msg)
		return
	}
	h.saveRecipe(w, r, img, nil, files, refs)
}

func (h *api) editRecipe(w http.ResponseWriter, r *http.Request) {
	img, ok := h.imageFromPath(w, r, "artifacts.images.update")
	if !ok {
		return
	}
	var body struct {
		Base int
		Put  []struct {
			Path    string
			Mode    uint32
			Content []byte
		}
		Delete []string
		Refs   *[]recipeRefJSON
	}
	// Base64 content needs 4/3 of the recipe limit.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRecipeBytes*4/3+1<<20)).Decode(&body); err != nil {
		he := bodyError(err, "body")
		writeError(w, he.status, he.msg)
		return
	}
	if body.Base != currentRevision(img) {
		writeError(w, http.StatusConflict, errRecipeChanged.msg)
		return
	}
	base := store.RecipeRevision{}
	if body.Base != 0 {
		var err error
		if base, err = h.store.RecipeRevision(r.Context(), img.ID, body.Base); err != nil {
			serverError(w, "recipe revision", err)
			return
		}
	}
	files := map[string]store.RecipeFile{}
	for _, f := range base.Files {
		files[f.Path] = f
	}
	for _, p := range body.Delete {
		if _, ok := files[p]; !ok {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("no file %q", p))
			return
		}
		delete(files, p)
	}
	for _, f := range body.Put {
		if f.Mode == 0 {
			f.Mode = 0o644
		}
		files[f.Path] = store.RecipeFile{Path: f.Path, Mode: f.Mode, Content: f.Content}
	}
	refs := base.Refs
	if body.Refs != nil {
		if refs, ok = h.checkRefs(w, r, img, *body.Refs); !ok {
			return
		}
	}
	list := make([]store.RecipeFile, 0, len(files))
	for _, f := range files {
		list = append(list, f)
	}
	h.saveRecipe(w, r, img, &body.Base, list, refs)
}

// saveRecipe stores files as the next revision; with base set, only if base is still the current one.
func (h *api) saveRecipe(w http.ResponseWriter, r *http.Request, img store.Image, base *int, files []store.RecipeFile, refs []store.RecipeRef) {
	if he := checkRecipe(files); he != nil {
		writeError(w, he.status, he.msg)
		return
	}
	ctx := r.Context()
	p, _ := auth.PrincipalFrom(ctx)
	var n int
	if !h.commit(w, r, func(tx *store.Store) error {
		cur, err := tx.LockImage(ctx, img.OrgID, img.ID)
		if err != nil {
			return err
		}
		if base != nil && *base != currentRevision(cur) {
			return errRecipeChanged
		}
		if n, err = tx.AddRecipeRevision(ctx, store.RecipeRevision{ImageID: img.ID, Files: files, Refs: refs, CreatedBy: p.User.ID}); err != nil {
			return err
		}
		e := imageAudit(r, store.ActionImageRecipe, img)
		e.Details["revision"], e.Details["files"], e.Details["refs"] = n, len(files), toRefJSON(refs)
		return tx.Audit(ctx, e)
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": n})
}

func (h *api) getRecipe(w http.ResponseWriter, r *http.Request) {
	img, ok := h.imageFromPath(w, r, "artifacts.images.list")
	if !ok {
		return
	}
	type fileJSON struct {
		Path    string  `json:"path"`
		Mode    uint32  `json:"mode"`
		Size    int     `json:"size"`
		Content *string `json:"content"`
	}
	files, refs := []fileJSON{}, []recipeRefJSON{}
	var createdAt *time.Time
	var createdBy *uuid.UUID
	if img.RecipeRevision != nil {
		rev, err := h.store.RecipeRevision(r.Context(), img.ID, *img.RecipeRevision)
		if err != nil {
			serverError(w, "recipe revision", err)
			return
		}
		for _, f := range rev.Files {
			j := fileJSON{Path: f.Path, Mode: f.Mode, Size: len(f.Content)}
			if len(f.Content) <= maxRecipeText && utf8.Valid(f.Content) {
				s := string(f.Content)
				j.Content = &s
			}
			files = append(files, j)
		}
		refs, createdAt, createdBy = toRefJSON(rev.Refs), &rev.CreatedAt, &rev.CreatedBy
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": img.RecipeRevision, "files": files, "refs": refs, "createdAt": createdAt, "createdBy": createdBy})
}

// ponytail: loads the whole revision to serve one file; add a single-file store query if recipes get big.
func (h *api) getRecipeFile(w http.ResponseWriter, r *http.Request) {
	img, ok := h.imageFromPath(w, r, "artifacts.images.list")
	if !ok {
		return
	}
	if img.RecipeRevision != nil {
		rev, err := h.store.RecipeRevision(r.Context(), img.ID, *img.RecipeRevision)
		if err != nil {
			serverError(w, "recipe revision", err)
			return
		}
		for _, f := range rev.Files {
			if f.Path == r.PathValue("path") {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("X-Content-Type-Options", "nosniff")
				w.Header().Set("Content-Disposition", "attachment")
				_, _ = w.Write(f.Content)
				return
			}
		}
	}
	writeError(w, http.StatusNotFound, "not found")
}

func (h *api) listImageVersions(w http.ResponseWriter, r *http.Request) {
	img, ok := h.imageFromPath(w, r, "artifacts.images.list")
	if !ok {
		return
	}
	vs, err := h.store.ImageVersions(r.Context(), img.ID)
	if err != nil {
		serverError(w, "image versions", err)
		return
	}
	tags, err := h.store.ImageTags(r.Context(), img.ID)
	if err != nil {
		serverError(w, "image tags", err)
		return
	}
	type versionJSON struct {
		Digest    string     `json:"digest"`
		Source    string     `json:"source"`
		Revision  *int       `json:"revision"`
		BuildID   *uuid.UUID `json:"buildId"`
		SizeBytes int64      `json:"sizeBytes"`
		CreatedAt time.Time  `json:"createdAt"`
		Tags      []string   `json:"tags"`
	}
	out := []versionJSON{}
	for _, v := range vs {
		j := versionJSON{v.Digest, v.Source, v.Revision, v.BuildID, v.SizeBytes, v.CreatedAt, []string{}}
		for tag, d := range tags {
			if d == v.Digest {
				j.Tags = append(j.Tags, tag)
			}
		}
		slices.Sort(j.Tags)
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) deleteImageVersion(w http.ResponseWriter, r *http.Request) {
	img, ok := h.imageFromPath(w, r, "artifacts.images.update")
	if !ok {
		return
	}
	digest := r.PathValue("digest")
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.DeleteImageVersion(r.Context(), img.ID, digest); err != nil {
			return err
		}
		e := imageAudit(r, store.ActionImageVersionDelete, img)
		e.Details["digest"] = digest
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	h.deleteManifests(r.Context(), img.ID, []string{digest})
	w.WriteHeader(http.StatusNoContent)
}

// tagFromPath refuses latest, which only builds and pushes move.
func tagFromPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	tag := r.PathValue("tag")
	if tag == latestTag || !tagPattern.MatchString(tag) {
		writeError(w, http.StatusUnprocessableEntity, "invalid tag; latest is set by builds and pushes")
		return "", false
	}
	return tag, true
}

func (h *api) setImageTag(w http.ResponseWriter, r *http.Request) {
	img, ok := h.imageFromPath(w, r, "artifacts.images.update")
	if !ok {
		return
	}
	tag, ok := tagFromPath(w, r)
	if !ok {
		return
	}
	var body struct{ Digest string }
	if !readJSON(w, r, &body) {
		return
	}
	h.commitTag(w, r, img, tag, body.Digest, func(tx *store.Store) error { return tx.SetImageTag(r.Context(), img.ID, tag, body.Digest) })
}

func (h *api) deleteImageTag(w http.ResponseWriter, r *http.Request) {
	img, ok := h.imageFromPath(w, r, "artifacts.images.update")
	if !ok {
		return
	}
	tag, ok := tagFromPath(w, r)
	if !ok {
		return
	}
	h.commitTag(w, r, img, tag, "", func(tx *store.Store) error { return tx.DeleteImageTag(r.Context(), img.ID, tag) })
}

func (h *api) commitTag(w http.ResponseWriter, r *http.Request, img store.Image, tag, digest string, fn func(tx *store.Store) error) {
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := fn(tx); err != nil {
			return err
		}
		e := imageAudit(r, store.ActionImageTag, img)
		e.Details["tag"], e.Details["digest"] = tag, digest
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	h.tags.Kick(img.ID)
	w.WriteHeader(http.StatusNoContent)
}

type defaultLevel struct {
	org   uuid.UUID
	scope store.EnvScope
	owner *uuid.UUID
	vmFor images.Owner
}

// defaultLevelFromPath reads org, team/{team} or user (always the caller's own) and checks act on that level.
func (h *api) defaultLevelFromPath(w http.ResponseWriter, r *http.Request, act string) (defaultLevel, bool) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return defaultLevel{}, false
	}
	p, _ := auth.PrincipalFrom(r.Context())
	l := defaultLevel{org: m.OrgID}
	switch {
	case r.PathValue("team") != "":
		id, ok := pathID(w, r, "team")
		if !ok {
			return l, false
		}
		if !h.z.Enforcer().HasTeam(m.OrgID, id) {
			writeError(w, http.StatusNotFound, "not found")
			return l, false
		}
		l.scope, l.owner, l.vmFor = store.EnvScopeTeam, &id, images.Owner{TeamID: &id}
	case r.PathValue("level") == "org":
		l.scope = store.EnvScopeOrg
	case r.PathValue("level") == "user":
		l.scope, l.owner, l.vmFor = store.EnvScopeUser, &p.User.ID, images.Owner{UserID: &p.User.ID}
	default:
		writeError(w, http.StatusNotFound, "not found")
		return l, false
	}
	return l, h.authorize(w, r, m.OrgID, imageObject(l.scope, l.owner), act)
}

func (h *api) resolver() images.Resolver {
	return images.Resolver{Store: h.store, IAM: h.iam, Authz: h.z}
}

func (h *api) getImageDefault(w http.ResponseWriter, r *http.Request) {
	l, ok := h.defaultLevelFromPath(w, r, "artifacts.images.list")
	if !ok {
		return
	}
	ctx := r.Context()
	p, _ := auth.PrincipalFrom(ctx)
	teams, err := h.teamNames(ctx, l.org)
	if err != nil {
		serverError(w, "teams", err)
		return
	}
	ds, err := h.store.ImageDefaults(ctx, l.org)
	if err != nil {
		serverError(w, "image defaults", err)
		return
	}
	type configuredJSON struct {
		Ref     string `json:"ref"`
		Removed bool   `json:"removed"`
	}
	var configured *configuredJSON
	for _, d := range ds {
		if d.Scope != l.scope || !sameOwner(d.OwnerID, l.owner) {
			continue
		}
		img, err := h.store.Image(ctx, l.org, d.ImageID)
		if err != nil {
			serverError(w, "image", err)
			return
		}
		// The row outlives the version or tag it names; the cascade then skips it.
		removed := false
		if !img.Platform {
			if d.Digest != "" {
				_, err = h.store.ImageVersion(ctx, img.ID, d.Digest)
			} else {
				_, err = h.store.ResolveImageTag(ctx, img.ID, cmp.Or(d.Tag, latestTag))
			}
			if removed = errors.Is(err, store.ErrNotFound); err != nil && !removed {
				serverError(w, "image default", err)
				return
			}
		}
		configured = &configuredJSON{imageRef(img, teams, d.Tag, d.Digest), removed}
	}
	res, err := h.resolver().Resolve(ctx, l.org, p.User.ID, l.vmFor, "")
	if err != nil {
		serverError(w, "resolve default", err)
		return
	}
	effective := map[string]any{"ref": platformRef, "source": res.Source, "digest": res.Digest}
	if res.Image != nil {
		ref := imageRef(*res.Image, teams, res.Tag, "")
		if res.Tag == "" {
			ref = imageRef(*res.Image, teams, "", res.Digest)
		}
		effective["ref"], effective["imageId"] = ref, res.Image.ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": configured, "effective": effective})
}

func (h *api) setImageDefault(w http.ResponseWriter, r *http.Request) {
	l, ok := h.defaultLevelFromPath(w, r, "artifacts.images.setDefault")
	if !ok {
		return
	}
	ctx := r.Context()
	var body struct{ Ref string }
	if !readJSON(w, r, &body) {
		return
	}
	parsed, err := images.ParseRef(body.Ref)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	p, _ := auth.PrincipalFrom(ctx)
	// The resolver only finds images the caller may use, so artifacts.images.use is checked here.
	res, err := h.resolver().Resolve(ctx, l.org, p.User.ID, l.vmFor, body.Ref)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, images.ErrAmbiguous) || errors.Is(err, images.ErrPlatformPinned) {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	} else if err != nil {
		serverError(w, "resolve image", err)
		return
	}
	d := store.ImageDefault{OrgID: l.org, Scope: l.scope, OwnerID: l.owner}
	if res.Image == nil {
		platform, err := h.store.PlatformImage(ctx)
		if err != nil {
			serverError(w, "platform image", err)
			return
		}
		d.ImageID = platform.ID
	} else {
		if !defaultSees(h.z.Enforcer(), l.scope, l.owner, *res.Image) {
			writeError(w, http.StatusUnprocessableEntity, "this image is not visible at this level")
			return
		}
		d.ImageID = res.Image.ID
		if parsed.Digest != "" {
			d.Digest = res.Digest
		} else {
			d.Tag = res.Tag
		}
	}
	h.commitDefault(w, r, l, func(tx *store.Store) error { return tx.SetImageDefault(ctx, d) }, map[string]any{"imageId": d.ImageID, "tag": d.Tag, "digest": d.Digest})
}

func (h *api) clearImageDefault(w http.ResponseWriter, r *http.Request) {
	l, ok := h.defaultLevelFromPath(w, r, "artifacts.images.setDefault")
	if !ok {
		return
	}
	h.commitDefault(w, r, l, func(tx *store.Store) error { return tx.ClearImageDefault(r.Context(), l.org, l.scope, l.owner) }, map[string]any{"cleared": true})
}

func (h *api) commitDefault(w http.ResponseWriter, r *http.Request, l defaultLevel, fn func(tx *store.Store) error, details map[string]any) {
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := fn(tx); err != nil {
			return err
		}
		e := auditEntry(r, &l.org, store.ActionImageDefault, imageObject(l.scope, l.owner), store.ResultAllowed)
		e.Details = details
		e.Details["scope"], e.Details["ownerId"] = l.scope, l.owner
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
