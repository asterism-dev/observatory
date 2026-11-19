package registry

import (
	"bytes"
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

// Manifest is an image manifest; an index resolves to its linux/amd64 child, while Blobs holds every child's layers.
type Manifest struct {
	Platform string
	Layers   []Layer
	Blobs    []Layer
}

var ErrNotFound = errors.New("not found in the registry")

const (
	// HubSubject is the registry actor of the hub's own requests, such as tag updates.
	HubSubject    = "hub"
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
		out, err := c.image(ctx, repo, m)
		out.Blobs = out.Layers
		return out, err
	}
	var out Manifest
	var platforms []string
	for _, d := range m.Manifests {
		var child manifestDoc
		if err := c.getJSON(ctx, repo, "/manifests/"+d.Digest, manifestTypes, &child); err != nil {
			return Manifest{}, err
		}
		for _, l := range child.Layers {
			out.Blobs = append(out.Blobs, Layer{Digest: l.Digest, Size: l.Size})
		}
		p := platform(d.Platform.OS, d.Platform.Architecture, d.Platform.Variant)
		if p != "linux/amd64" || out.Platform != "" {
			platforms = append(platforms, p)
			continue
		}
		img, err := c.image(ctx, repo, child)
		if err != nil {
			return Manifest{}, err
		}
		out.Platform, out.Layers = img.Platform, img.Layers
	}
	if out.Platform == "" {
		out.Platform = strings.Join(platforms, ", ")
	}
	return out, nil
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

// PutTag points tag at the manifest digest, re-uploading the manifest under the tag unless the tag already resolves to digest.
func (c Client) PutTag(ctx context.Context, repo, tag, digest string) error {
	head, err := c.do(ctx, http.MethodHead, repo, "/manifests/"+tag, manifestTypes, "pull")
	if err != nil {
		return err
	}
	head.Body.Close()
	if head.StatusCode == http.StatusOK && head.Header.Get("Docker-Content-Digest") == digest {
		return nil
	}
	get, err := c.do(ctx, http.MethodGet, repo, "/manifests/"+digest, manifestTypes, "pull")
	if err != nil {
		return err
	}
	defer get.Body.Close()
	if get.StatusCode != http.StatusOK {
		return fmt.Errorf("get manifest %s@%s: %s", repo, digest, get.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(get.Body, maxDocument))
	if err != nil {
		return err
	}
	put, err := c.send(ctx, http.MethodPut, repo, "/manifests/"+tag, http.Header{"Content-Type": {get.Header.Get("Content-Type")}}, bytes.NewReader(raw), "pull", "push")
	if err != nil {
		return err
	}
	put.Body.Close()
	if put.StatusCode != http.StatusCreated {
		return fmt.Errorf("put tag %s:%s: %s", repo, tag, put.Status)
	}
	return nil
}

func (c Client) do(ctx context.Context, method, repo, path, accept, action string) (*http.Response, error) {
	h := http.Header{}
	if accept != "" {
		h.Set("Accept", accept)
	}
	return c.send(ctx, method, repo, path, h, nil, action)
}

func (c Client) send(ctx context.Context, method, repo, path string, h http.Header, body io.Reader, actions ...string) (*http.Response, error) {
	tok, err := c.Issuer.Token(HubSubject, []Access{{Type: "repository", Name: repo, Actions: actions}}, TokenTTL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+"/v2/"+repo+path, body)
	if err != nil {
		return nil, err
	}
	req.Header = h
	req.Header.Set("Authorization", "Bearer "+tok)
	return httpClient.Do(req)
}
