package api

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/asterism-dev/observatory/internal/store"
)

var (
	errSourceNotFound    = errors.New("github repository, ref or path not found")
	errSourceUnreachable = errors.New("github is unreachable")
)

var (
	ghOwnerRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	ghNameRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	ghRefRE   = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	ghSHARE   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type gitHubSource struct {
	API    string
	Client *http.Client
}

func (g gitHubSource) get(ctx context.Context, token, urlPath, accept string) (*http.Response, error) {
	api := g.API
	if api == "" {
		api = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(api, "/")+urlPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	c := g.Client
	if c == nil {
		// Go drops Authorization when a redirect leaves the API host.
		c = &http.Client{Timeout: 40 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errSourceUnreachable, errors.Unwrap(err))
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return resp, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity:
		err = errSourceNotFound
	case resp.StatusCode >= 500:
		err = errSourceUnreachable
	default:
		err = fmt.Errorf("github answered %s", resp.Status)
	}
	resp.Body.Close()
	return nil, err
}

func (g gitHubSource) resolve(ctx context.Context, token, repo, ref string) (string, error) {
	if ref == "" {
		ref = "HEAD"
	}
	resp, err := g.get(ctx, token, "/repos/"+repo+"/commits/"+url.PathEscape(ref), "application/vnd.github.sha")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if sha := strings.TrimSpace(string(body)); err == nil && ghSHARE.MatchString(sha) {
		return sha, nil
	}
	return "", fmt.Errorf("%w: unexpected commit answer", errSourceUnreachable)
}

// files returns the regular files under path, relative to it, and a log line per skipped entry.
func (g gitHubSource) files(ctx context.Context, token, repo, sha, path string) ([]store.RecipeFile, []string, error) {
	if path != "" && !validRecipePath(path) {
		return nil, nil, fmt.Errorf("invalid path %q", path)
	}
	resp, err := g.get(ctx, token, "/repos/"+repo+"/tarball/"+url.PathEscape(sha), "application/vnd.github+json")
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", errSourceUnreachable, err)
	}
	tr := tar.NewReader(zr)
	var files []store.RecipeFile
	var logs []string
	total := int64(0)
	for entries := 0; ; entries++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, logs, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", errSourceUnreachable, err)
		}
		if entries > 2*maxRecipeFiles || len(files) > maxRecipeFiles {
			return nil, nil, fmt.Errorf("a recipe holds at most %d files", maxRecipeFiles)
		}
		switch hdr.Typeflag {
		case tar.TypeDir, tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		case tar.TypeReg:
		default:
			logs = append(logs, fmt.Sprintf("skipped %q: not a regular file", hdr.Name))
			continue
		}
		if !validRecipePath(hdr.Name) {
			logs = append(logs, fmt.Sprintf("skipped %q: invalid path", hdr.Name))
			continue
		}
		rel, ok := relativeTo(hdr.Name, path)
		if !ok {
			continue
		}
		if total += hdr.Size; total > maxRecipeBytes {
			return nil, nil, errors.New("a recipe holds at most 50 MB")
		}
		content, err := io.ReadAll(io.LimitReader(tr, maxRecipeBytes+1))
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", errSourceUnreachable, err)
		}
		mode := uint32(0o644)
		if hdr.Mode&0o111 != 0 {
			mode = 0o755
		}
		files = append(files, store.RecipeFile{Path: rel, Mode: mode, Content: content})
	}
}

// relativeTo drops the archive's top directory and the source path; ok is false outside path.
func relativeTo(name, path string) (string, bool) {
	_, rest, found := strings.Cut(name, "/")
	if !found {
		return "", false
	}
	if path == "" {
		return rest, true
	}
	return strings.CutPrefix(rest, path+"/")
}

func parseGitHubSource(s string) (store.ImageSource, error) {
	var src store.ImageSource
	rest, path, hasPath := strings.Cut(s, ":")
	repo, ref, hasRef := strings.Cut(rest, "@")
	owner, name, ok := strings.Cut(repo, "/")
	switch {
	case !ok || !ghOwnerRE.MatchString(owner) || !ghNameRE.MatchString(name) || name == "." || name == "..":
		return src, errors.New("a source reads owner/repo[@ref][:path]")
	case hasRef && (!ghRefRE.MatchString(ref) || strings.Contains(ref, "..")):
		return src, errors.New("invalid ref")
	case hasPath && !validRecipePath(path):
		return src, errors.New("invalid path")
	}
	return store.ImageSource{Repo: repo, Ref: ref, Path: path}, nil
}
