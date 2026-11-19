package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to the registry's internal URL as the hub itself.
type Client struct {
	URL    string
	Issuer Issuer
}

type Layer struct {
	Digest string
	Size   int64
}

// Manifest is an image manifest; an index resolves to its linux/amd64 child.
type Manifest struct {
	Platform string
	Layers   []Layer
}

var ErrNotFound = errors.New("not found in the registry")

const (
	hubSubject    = "hub"
	maxDocument   = 4 << 20
	manifestTypes = "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, " +
		"application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// DeleteManifest treats an already missing manifest as deleted.
func (c Client) DeleteManifest(ctx context.Context, repo, digest string) error {
	resp, err := c.do(ctx, http.MethodDelete, repo, "/manifests/"+digest, "", "delete")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete manifest %s@%s: %s", repo, digest, resp.Status)
	}
	return nil
}

type manifestDoc struct {
	Config struct{ Digest string }
	Layers []struct {
		Digest string
		Size   int64
	}
	Manifests []struct {
		Digest   string
		Platform struct{ OS, Architecture, Variant string }
	}
}

// Manifest of an index without a linux/amd64 child lists the index's platforms as Platform.
func (c Client) Manifest(ctx context.Context, repo, digest string) (Manifest, error) {
	var m manifestDoc
	if err := c.getJSON(ctx, repo, "/manifests/"+digest, manifestTypes, &m); err != nil {
		return Manifest{}, err
	}
	if m.Manifests == nil {
		return c.image(ctx, repo, m)
	}
	var platforms []string
	for _, d := range m.Manifests {
		p := platform(d.Platform.OS, d.Platform.Architecture, d.Platform.Variant)
		if p == "linux/amd64" {
			var child manifestDoc
			if err := c.getJSON(ctx, repo, "/manifests/"+d.Digest, manifestTypes, &child); err != nil {
				return Manifest{}, err
			}
			return c.image(ctx, repo, child)
		}
		platforms = append(platforms, p)
	}
	return Manifest{Platform: strings.Join(platforms, ", ")}, nil
}

// image takes the platform from the config blob; a nested index yields an empty platform.
func (c Client) image(ctx context.Context, repo string, m manifestDoc) (Manifest, error) {
	if m.Manifests != nil || m.Config.Digest == "" {
		return Manifest{}, nil
	}
	var cfg struct{ OS, Architecture, Variant string }
	if err := c.getJSON(ctx, repo, "/blobs/"+m.Config.Digest, "", &cfg); err != nil {
		return Manifest{}, err
	}
	out := Manifest{Platform: platform(cfg.OS, cfg.Architecture, cfg.Variant)}
	for _, l := range m.Layers {
		out.Layers = append(out.Layers, Layer{Digest: l.Digest, Size: l.Size})
	}
	return out, nil
}

func platform(os, arch, variant string) string {
	if variant != "" {
		return os + "/" + arch + "/" + variant
	}
	return os + "/" + arch
}

func (c Client) getJSON(ctx context.Context, repo, path, accept string, v any) error {
	resp, err := c.do(ctx, http.MethodGet, repo, path, accept, "pull")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("get %s%s: %w", repo, path, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %s%s: %s", repo, path, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxDocument)).Decode(v)
}

func (c Client) do(ctx context.Context, method, repo, path, accept, action string) (*http.Response, error) {
	tok, err := c.Issuer.Token(hubSubject, []Access{{Type: "repository", Name: repo, Actions: []string{action}}}, TokenTTL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+"/v2/"+repo+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return httpClient.Do(req)
}
