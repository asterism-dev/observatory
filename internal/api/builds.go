package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/builds"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

const (
	maxBuildLogPost = 8 << 20
	maxBases        = 100
)

var (
	digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	// logPollInterval is how often a followed log looks for new output.
	logPollInterval = time.Second
)

type buildJSON struct {
	ID         uuid.UUID  `json:"id"`
	ImageID    uuid.UUID  `json:"imageId"`
	Revision   int        `json:"revision"`
	State      string     `json:"state"`
	StartedBy  uuid.UUID  `json:"startedBy"`
	Automatic  bool       `json:"automatic"`
	QueuedAt   time.Time  `json:"queuedAt"`
	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	Digest     string     `json:"digest"`
	Error      string     `json:"error"`
	Source     string     `json:"source"`
}

func toBuildJSON(b store.ImageBuild) buildJSON {
	return buildJSON{b.ID, b.ImageID, b.Revision, b.State, b.StartedBy, b.Automatic, b.QueuedAt, b.StartedAt, b.FinishedAt, b.Digest, b.Error, b.Source}
}

func buildAudit(r *http.Request, action string, img store.Image, b store.ImageBuild) store.AuditEntry {
	e := imageAudit(r, action, img)
	e.Details["buildId"], e.Details["revision"] = b.ID, b.Revision
	return e
}

func (h *api) startBuild(w http.ResponseWriter, r *http.Request) {
	img, ok := h.imageFromPath(w, r, "image:build")
	if !ok {
		return
	}
	if h.builds == nil {
		writeError(w, http.StatusServiceUnavailable, "image builds need the registry, which this hub has not configured")
		return
	}
	if img.RecipeRevision == nil && img.Source == (store.ImageSource{}) {
		writeError(w, http.StatusUnprocessableEntity, "the image has no recipe")
		return
	}
	ctx := r.Context()
	p, _ := auth.PrincipalFrom(ctx)
	sub := authz.UserSubject(p.User.ID)
	rev, fetchLog, err := fetchSource(ctx, h.store, h.github, func(obj, act string) bool { return h.z.Enforcer().Allowed(sub, img.OrgID, obj, act) }, img, p.User.ID)
	var he httpError
	if errors.As(err, &he) {
		writeError(w, he.status, he.msg)
		return
	} else if err != nil {
		serverError(w, "recipe revision", err)
		return
	}
	// An edit keeps references someone else added, so the starter's use right is checked here, not at edit time.
	if !h.checkRefUse(w, r, img, rev.Refs) {
		return
	}
	if over, err := overImageStorageLimit(ctx, h.store, img.OrgID); err != nil {
		serverError(w, "image storage", err)
		return
	} else if over {
		writeError(w, http.StatusConflict, "the org is over its image storage limit")
		return
	}
	var b store.ImageBuild
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if b, err = tx.CreateImageBuild(ctx, store.ImageBuild{ImageID: img.ID, Revision: rev.Number, StartedBy: p.User.ID}); errors.Is(err, store.ErrBuildRunning) {
			return httpError{http.StatusConflict, err.Error()}
		} else if err != nil {
			return err
		}
		return tx.Audit(ctx, buildAudit(r, store.ActionImageBuild, img, b))
	}) {
		return
	}
	appendFetchLog(ctx, h.store, b.ID, fetchLog)
	h.builds.Kick()
	writeJSON(w, http.StatusAccepted, map[string]any{"buildId": b.ID})
}

// checkRefUse requires the caller's use right on every reference and that each still exists.
func (h *api) checkRefUse(w http.ResponseWriter, r *http.Request, img store.Image, refs []store.RecipeRef) bool {
	denied := false
	msg, err := refsUsable(r.Context(), h.store, img.OrgID, refs, func(obj, act string) bool {
		denied = !h.authorize(w, r, img.OrgID, obj, act)
		return !denied
	})
	switch {
	case denied:
	case err != nil:
		serverError(w, "recipe references", err)
	case msg != "":
		writeError(w, http.StatusUnprocessableEntity, msg)
	default:
		return true
	}
	return false
}

// refsUsable returns why refs cannot be used in a build: a use right allowed denies or a reference that no longer exists.
func refsUsable(ctx context.Context, s *store.Store, org uuid.UUID, refs []store.RecipeRef, allowed func(obj, act string) bool) (string, error) {
	envs, err := s.EnvVars(ctx, org)
	if err != nil {
		return "", err
	}
	igs, err := s.Integrations(ctx, org)
	if err != nil {
		return "", err
	}
	for _, ref := range refs {
		var found bool
		if ref.Kind == "secret" {
			if !allowed(envObject(ref.Scope, ref.OwnerID), "env:use") {
				return "no right to use " + describeRef(ref), nil
			}
			for _, v := range envs {
				found = found || v.Scope == ref.Scope && sameOwner(v.OwnerID, ref.OwnerID) && v.Name == ref.Name
			}
		} else {
			if !allowed(integrationObject(ref.Scope, ref.OwnerID), "integration:use") {
				return "no right to use " + describeRef(ref), nil
			}
			for _, v := range igs {
				found = found || v.Scope == ref.Scope && sameOwner(v.OwnerID, ref.OwnerID) && string(v.Kind) == ref.Name
			}
		}
		if !found {
			return "the recipe references " + describeRef(ref) + ", which no longer exists", nil
		}
	}
	return "", nil
}

func (h *api) listBuilds(w http.ResponseWriter, r *http.Request) {
	img, ok := h.imageFromPath(w, r, "image:list")
	if !ok {
		return
	}
	bs, err := h.store.ImageBuilds(r.Context(), img.ID)
	if err != nil {
		serverError(w, "builds", err)
		return
	}
	out := make([]buildJSON, 0, len(bs))
	for _, b := range bs {
		out = append(out, toBuildJSON(b))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) buildFromPath(w http.ResponseWriter, r *http.Request, act string) (store.Image, store.ImageBuild, bool) {
	img, ok := h.imageFromPath(w, r, act)
	if !ok {
		return img, store.ImageBuild{}, false
	}
	id, ok := pathID(w, r, "bid")
	if !ok {
		return img, store.ImageBuild{}, false
	}
	b, err := h.store.ImageBuild(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || err == nil && b.ImageID != img.ID {
		writeError(w, http.StatusNotFound, "not found")
		return img, b, false
	} else if err != nil {
		serverError(w, "build", err)
		return img, b, false
	}
	return img, b, true
}

func (h *api) getBuild(w http.ResponseWriter, r *http.Request) {
	if _, b, ok := h.buildFromPath(w, r, "image:list"); ok {
		writeJSON(w, http.StatusOK, toBuildJSON(b))
	}
}

func (h *api) cancelBuild(w http.ResponseWriter, r *http.Request) {
	img, b, ok := h.buildFromPath(w, r, "image:build")
	if !ok {
		return
	}
	if h.builds == nil {
		writeError(w, http.StatusServiceUnavailable, "image builds need the registry, which this hub has not configured")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	b, err := h.builds.Finish(r.Context(), b.ID, store.BuildCancelled, "", "cancelled by "+p.User.Name)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "the build already ended")
		return
	} else if err != nil {
		serverError(w, "cancel build", err)
		return
	}
	if err := h.store.Audit(r.Context(), buildAudit(r, store.ActionImageBuildCancel, img, b)); err != nil {
		slog.Error("audit build cancel", "build", b.ID, "err", err)
	}
	writeJSON(w, http.StatusOK, toBuildJSON(b))
}

func (h *api) buildLog(w http.ResponseWriter, r *http.Request) {
	_, b, ok := h.buildFromPath(w, r, "image:list")
	if !ok {
		return
	}
	if r.URL.Query().Get("follow") == "1" {
		h.followBuildLog(w, r, b.ID)
		return
	}
	raw, err := h.store.BuildLog(r.Context(), b.ID)
	if err != nil {
		serverError(w, "build log", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(raw)
}

// followBuildLog streams the log as server-sent events, one data line per log line, and an end event with the final state.
func (h *api) followBuildLog(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	ctx := r.Context()
	rc := http.NewResponseController(w)
	// A build runs for up to an hour, longer than the server's write timeout.
	_ = rc.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	var partial []byte
	sent, after := 0, int64(0)
	emit := func(chunk []byte, final bool) {
		sent += len(chunk)
		partial = append(partial, chunk...)
		for {
			i := bytes.IndexByte(partial, '\n')
			if i < 0 {
				break
			}
			fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(string(partial[:i]), "\r", ""))
			partial = partial[i+1:]
		}
		if final && len(partial) > 0 {
			fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(string(partial), "\r", ""))
		}
	}
	for {
		chunk, last, finished, err := h.store.BuildLogSince(ctx, id, after)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("follow build log", "build", id, "err", err)
			}
			return
		}
		after = last
		if finished {
			full, err := h.store.BuildLog(ctx, id)
			b, berr := h.store.ImageBuild(ctx, id)
			if err != nil || berr != nil {
				slog.Error("follow build log", "build", id, "err", errors.Join(err, berr))
				return
			}
			// ponytail: a log cut on finishing (over 10 MB) no longer lines up with what was sent; the rest is skipped then.
			if sent <= len(full) {
				emit(full[sent:], true)
			}
			fmt.Fprintf(w, "event: end\ndata: %s\n\n", b.State)
			_ = rc.Flush()
			return
		}
		emit(chunk, false)
		_ = rc.Flush()
		select {
		case <-ctx.Done():
			return
		case <-time.After(logPollInterval):
		}
	}
}

func (h *api) listBuilderVMs(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	if !privileged(m.Role) {
		h.auditDenied(r, m.Org.ID, authz.ObjVMs, "vm:builders")
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	vms, err := h.store.BuilderVMs(r.Context(), m.Org.ID)
	if err != nil {
		serverError(w, "builder vms", err)
		return
	}
	names, err := h.vmNames(r, m.Org.ID)
	if err != nil {
		serverError(w, "vm names", err)
		return
	}
	out := []vmJSON{}
	for _, v := range vms {
		out = append(out, h.vmJSON(r, v, names))
	}
	writeJSON(w, http.StatusOK, map[string]any{"vms": out})
}

// BuildEndpoints serves the build agent in builder VMs, which authenticates with its build secret.
func BuildEndpoints(mux *http.ServeMux, s *store.Store, b *builds.Scheduler, manifests RegistryManifests, tags *TagSync, logs *telemetry.LogWriter) {
	a := &buildAgent{store: s, builds: b, events: &registryEvents{store: s, manifests: manifests, tags: tags}}
	if logs != nil {
		a.copier = newBuildLogCopier(logs, s)
	}
	mux.HandleFunc("GET /api/builds/{id}/context", a.context)
	mux.HandleFunc("POST /api/builds/{id}/log", a.log)
	mux.HandleFunc("POST /api/builds/{id}/result", a.result)
}

type buildAgent struct {
	store  *store.Store
	builds *builds.Scheduler
	events *registryEvents
	copier *buildLogCopier
}

func (a *buildAgent) build(w http.ResponseWriter, r *http.Request) (store.ImageBuild, bool) {
	secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if ok && secret != "" {
		b, err := a.store.BuildBySecret(r.Context(), secret)
		if err == nil && b.ID.String() == r.PathValue("id") {
			return b, true
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			serverError(w, "build by secret", err)
			return b, false
		}
	}
	writeError(w, http.StatusUnauthorized, "unauthorized")
	return store.ImageBuild{}, false
}

// context sends the build's recipe revision as tar.gz.
func (a *buildAgent) context(w http.ResponseWriter, r *http.Request) {
	b, ok := a.build(w, r)
	if !ok {
		return
	}
	rev, err := a.store.RecipeRevision(r.Context(), b.ImageID, b.Revision)
	if err != nil {
		serverError(w, "recipe revision", err)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)
	for _, f := range rev.Files {
		hdr := &tar.Header{Name: f.Path, Mode: int64(f.Mode & 0o777), Size: int64(len(f.Content)), Typeflag: tar.TypeReg, ModTime: rev.CreatedAt}
		if err := tw.WriteHeader(hdr); err != nil {
			return
		}
		if _, err := tw.Write(f.Content); err != nil {
			return
		}
	}
	if err := tw.Close(); err == nil {
		_ = zw.Close()
	}
}

func (a *buildAgent) log(w http.ResponseWriter, r *http.Request) {
	b, ok := a.build(w, r)
	if !ok {
		return
	}
	chunk, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBuildLogPost))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "log chunk too large")
		return
	}
	accepted, err := a.store.AppendBuildLog(r.Context(), b.ID, chunk)
	if errors.Is(err, store.ErrBuildLogFinished) {
		writeError(w, http.StatusConflict, err.Error())
		return
	} else if err != nil {
		serverError(w, "append build log", err)
		return
	}
	if a.copier != nil {
		a.copier.chunk(r.Context(), b, chunk[:accepted])
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *buildAgent) result(w http.ResponseWriter, r *http.Request) {
	b, ok := a.build(w, r)
	if !ok {
		return
	}
	var body struct {
		Digest, Error string
		Bases         []store.BaseDigest
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	state, digest, msg := store.BuildFailed, "", body.Error
	switch {
	case body.Error != "":
	case !digestPattern.MatchString(body.Digest):
		writeError(w, http.StatusBadRequest, "invalid digest")
		return
	case !validBases(body.Bases):
		writeError(w, http.StatusBadRequest, "invalid bases")
		return
	default:
		var err error
		if msg, err = a.recordVersion(r.Context(), b, body.Digest); err != nil {
			serverError(w, "record build version", err)
			return
		}
		if msg == "" {
			state, digest = store.BuildSucceeded, body.Digest
			if err := a.store.SetVersionBaseDigests(r.Context(), b.ImageID, digest, body.Bases); err != nil {
				serverError(w, "base digests", err)
				return
			}
		}
	}
	if _, err := a.builds.Finish(r.Context(), b.ID, state, digest, msg); errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "the build already ended")
		return
	} else if err != nil {
		serverError(w, "finish build", err)
		return
	}
	if a.copier != nil {
		a.copier.finish(b)
	}
	if state == store.BuildSucceeded {
		a.events.tags.Kick(b.ImageID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func validBases(bases []store.BaseDigest) bool {
	if len(bases) > maxBases {
		return false
	}
	for _, b := range bases {
		if b.Ref == "" || len(b.Ref) > 512 || !digestPattern.MatchString(b.Digest) {
			return false
		}
	}
	return true
}

// recordVersion ingests the pushed digest as the registry's push event would, so it does not matter which arrives first; it returns why the push was refused.
func (a *buildAgent) recordVersion(ctx context.Context, b store.ImageBuild, digest string) (string, error) {
	var ev registryEvent
	ev.Target.Repository, ev.Target.Digest, ev.Target.Tag = imageRepo(b.ImageID), digest, "build-"+b.ID.String()
	ev.Actor.Name = "build:" + b.ID.String()
	if err := a.events.push(ctx, ev); err != nil {
		return "", err
	}
	_, err := a.store.ImageVersion(ctx, b.ImageID, digest)
	if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	img, err := a.store.ImageByID(ctx, b.ImageID)
	if err != nil {
		return "", err
	}
	if img.PushError != "" {
		return "the pushed image was refused: " + img.PushError, nil
	}
	return "the pushed image " + digest + " is not in the registry", nil
}
