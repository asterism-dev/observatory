package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/store"
)

const sourceFetchTimeout = 40 * time.Second

// fetchSource makes the newest commit of img's source its current revision; failures the user can act on are httpErrors.
func fetchSource(ctx context.Context, st *store.Store, gh gitHubSource, allowed func(obj, act string) bool, img store.Image, runner uuid.UUID) (store.RecipeRevision, []string, error) {
	var cur store.RecipeRevision
	if img.RecipeRevision != nil {
		var err error
		if cur, err = st.RecipeRevision(ctx, img.ID, *img.RecipeRevision); err != nil {
			return cur, nil, err
		}
	}
	src := img.Source
	if src == (store.ImageSource{}) {
		return cur, nil, nil
	}
	token, err := sourceToken(ctx, st, allowed, img)
	if err != nil {
		return cur, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, sourceFetchTimeout)
	defer cancel()
	sha, err := gh.resolve(ctx, token, src.Repo, src.Ref)
	if err != nil {
		return cur, nil, sourceFailure(src, token, err)
	}
	ref := src.Ref
	if ref == "" {
		ref = "default branch"
	}
	log := []string{fmt.Sprintf("fetching %s@%s → %s", src.Repo, ref, sha[:7])}
	source := fmt.Sprintf("github:%s@%s:%s", src.Repo, sha, src.Path)
	if img.RecipeRevision != nil && cur.Source == source {
		return cur, append(log, fmt.Sprintf("unchanged since revision %d", cur.Number)), nil
	}
	files, skipped, err := gh.files(ctx, token, src.Repo, sha, src.Path)
	if err != nil {
		return cur, nil, sourceFailure(src, token, err)
	}
	if he := checkRecipe(files); he != nil {
		return cur, nil, *he
	}
	rev := store.RecipeRevision{ImageID: img.ID, Files: files, Refs: cur.Refs, Source: source, CreatedBy: runner}
	err = st.InTx(ctx, func(tx *store.Store) error {
		locked, err := tx.LockImage(ctx, img.OrgID, img.ID)
		if err != nil {
			return err
		}
		// Refs are carried from cur, so a concurrent edit or source change must not be overwritten.
		if locked.Source != src || currentRevision(locked) != currentRevision(img) {
			return errRecipeChanged
		}
		rev.Number, err = tx.AddRecipeRevision(ctx, rev)
		return err
	})
	if err != nil {
		return cur, nil, err
	}
	log = append(log, skipped...)
	return rev, append(log, fmt.Sprintf("revision %d from %s", rev.Number, sha[:7])), nil
}

// sourceToken returns the token of the narrowest GitHub integration the runner may use for img, empty for none.
func sourceToken(ctx context.Context, st *store.Store, allowed func(obj, act string) bool, img store.Image) (string, error) {
	igs, err := st.Integrations(ctx, img.OrgID)
	if err != nil {
		return "", err
	}
	igs = slices.DeleteFunc(igs, func(v store.Integration) bool {
		return v.Kind != integrations.GitHub || !refAllowed(store.RecipeRef{Scope: v.Scope, OwnerID: v.OwnerID}, img.Scope, img.OwnerID) ||
			!allowed(integrationObject(v.Scope, v.OwnerID), "integration:use")
	})
	if len(igs) == 0 {
		return "", nil
	}
	v := slices.MinFunc(igs, func(a, b store.Integration) int {
		return cmp.Or(cmp.Compare(scopeRank(a.Scope), scopeRank(b.Scope)), cmp.Compare(ownerKey(a.OwnerID), ownerKey(b.OwnerID)))
	})
	return st.IntegrationToken(ctx, v)
}

func ownerKey(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func sourceFailure(src store.ImageSource, token string, err error) error {
	what := "fetching " + sourceString(src)
	switch {
	case errors.Is(err, errSourceNotFound) && token == "":
		return httpError{http.StatusUnprocessableEntity, what + ": not found or no access; connect a GitHub integration for private repositories"}
	case errors.Is(err, errSourceNotFound):
		return httpError{http.StatusUnprocessableEntity, what + ": not found or no access"}
	case errors.Is(err, errSourceUnreachable):
		return httpError{http.StatusBadGateway, what + ": " + err.Error()}
	}
	return httpError{http.StatusUnprocessableEntity, what + ": " + err.Error()}
}

func appendFetchLog(ctx context.Context, st *store.Store, build uuid.UUID, lines []string) {
	if len(lines) == 0 {
		return
	}
	if _, err := st.AppendBuildLog(ctx, build, []byte(strings.Join(lines, "\n")+"\n")); err != nil {
		slog.Error("append fetch log", "build", build, "err", err)
	}
}
