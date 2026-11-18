// Package guestimage defines the guest image manifest, its validation and the image version.
package guestimage

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const ImageFile = "/etc/observatory/image"

type Artifact struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
}

type Repo struct {
	Name        string            `json:"name"`
	URL         string            `json:"url"`
	Suite       string            `json:"suite"`
	Component   string            `json:"component"`
	KeyURL      string            `json:"keyURL"`
	Fingerprint string            `json:"fingerprint"`
	Packages    map[string]string `json:"packages"`
}

type Manifest struct {
	Release     string     `json:"release"`
	AptSnapshot string     `json:"aptSnapshot"`
	Base        Artifact   `json:"base"`
	Apt         []string   `json:"apt"`
	Repos       []Repo     `json:"repos"`
	Files       []Artifact `json:"files"`
}

type Image struct {
	Manifest Manifest
	Version  string
	Files    map[string][]byte
}

var (
	releaseRe     = regexp.MustCompile(`^\d{4}\.\d{1,2}\.\d+$`)
	snapshotRe    = regexp.MustCompile(`^\d{8}T\d{6}Z$`)
	sha256Re      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	fingerprintRe = regexp.MustCompile(`^[0-9A-F]{40}$`)

	fileNames = []string{"node", "claude", "codex", "uv", "chrome"}
	repoNames = []string{"docker", "gh", "xpra"}
)

func ParseManifest(b []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (m Manifest) Validate() error {
	if !releaseRe.MatchString(m.Release) {
		return fmt.Errorf("manifest: release %q is not YYYY.M.N", m.Release)
	}
	if !snapshotRe.MatchString(m.AptSnapshot) {
		return fmt.Errorf("manifest: aptSnapshot %q is not YYYYMMDDTHHMMSSZ", m.AptSnapshot)
	}
	if err := m.Base.validate("base"); err != nil {
		return err
	}
	for i, f := range m.Files {
		if err := f.validate(fmt.Sprintf("files[%d]", i)); err != nil {
			return err
		}
	}
	if err := exactNames("files", fileNames, len(m.Files), func(i int) string { return m.Files[i].Name }); err != nil {
		return err
	}
	for i, r := range m.Repos {
		if err := r.validate(fmt.Sprintf("repos[%d]", i)); err != nil {
			return err
		}
	}
	return exactNames("repos", repoNames, len(m.Repos), func(i int) string { return m.Repos[i].Name })
}

func exactNames(field string, want []string, n int, name func(int) string) error {
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		if seen[name(i)] {
			return fmt.Errorf("manifest: %s: duplicate name %q", field, name(i))
		}
		seen[name(i)] = true
	}
	for _, w := range want {
		if !seen[w] {
			return fmt.Errorf("manifest: %s: missing %s", field, w)
		}
	}
	for k := range seen {
		if !slices.Contains(want, k) {
			return fmt.Errorf("manifest: %s: unexpected %q", field, k)
		}
	}
	return nil
}

func (a Artifact) validate(field string) error {
	if a.Name == "" {
		return fmt.Errorf("manifest: %s.name is empty", field)
	}
	if a.Version == "" || strings.Contains(a.Version, "latest") {
		return fmt.Errorf("manifest: %s.version %q is not pinned", field, a.Version)
	}
	if err := httpsURL(field+".url", a.URL); err != nil {
		return err
	}
	if !sha256Re.MatchString(a.SHA256) {
		return fmt.Errorf("manifest: %s.sha256 must be 64 lowercase hex characters", field)
	}
	return nil
}

func (r Repo) validate(field string) error {
	if err := httpsURL(field+".url", r.URL); err != nil {
		return err
	}
	if err := httpsURL(field+".keyURL", r.KeyURL); err != nil {
		return err
	}
	if !fingerprintRe.MatchString(r.Fingerprint) {
		return fmt.Errorf("manifest: %s.fingerprint must be 40 uppercase hex characters", field)
	}
	if len(r.Packages) == 0 {
		return fmt.Errorf("manifest: %s.packages is empty", field)
	}
	for pkg, v := range r.Packages {
		if v == "" || v == "*" || strings.Contains(v, "latest") {
			return fmt.Errorf("manifest: %s.packages[%s] version %q is not pinned", field, pkg, v)
		}
	}
	return nil
}

func httpsURL(field, u string) error {
	if !strings.HasPrefix(u, "https://") || strings.Contains(u, "latest") {
		return fmt.Errorf("manifest: %s %q must be an https URL without \"latest\"", field, u)
	}
	return nil
}

func NewImage(files map[string][]byte) (Image, error) {
	raw, ok := files["manifest.json"]
	if !ok {
		return Image{}, fmt.Errorf("guestimage: manifest.json missing")
	}
	m, err := ParseManifest(raw)
	if err != nil {
		return Image{}, err
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	h := sha256.New()
	for _, n := range names {
		h.Write([]byte(n))
		h.Write([]byte{0})
		var l [8]byte
		binary.BigEndian.PutUint64(l[:], uint64(len(files[n])))
		h.Write(l[:])
		h.Write([]byte{0})
		h.Write(files[n])
	}
	h.Write([]byte(m.Base.SHA256))
	return Image{Manifest: m, Version: m.Release + "-" + hex.EncodeToString(h.Sum(nil))[:8], Files: files}, nil
}
