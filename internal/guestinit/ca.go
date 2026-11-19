package guestinit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	maxCAPEM     = 16 << 10
	caBlockBegin = "# BEGIN observatory egress\n"
	caBlockEnd   = "# END observatory egress\n"
	defaultCAs   = "etc/ssl/certs/ca-certificates.crt"
)

var (
	caBundles = []string{defaultCAs, "etc/pki/tls/certs/ca-bundle.crt"}
	caID      = regexp.MustCompile(`^[0-9a-f-]{36}$`)
	caPEM     = regexp.MustCompile(`^(-----BEGIN CERTIFICATE-----\r?\n[A-Za-z0-9+/=\r\n]+-----END CERTIFICATE-----\r?\n?)+$`)
)

// InstallCAs sets the marked egress block in every CA bundle of the image and returns the bundle path as the guest sees it.
func (r Root) InstallCAs(certs map[string]string) (string, error) {
	// Only public CA certs may land in the trust store: a key or any other PEM block is refused before anything is written.
	ids := make([]string, 0, len(certs))
	for id, pem := range certs {
		if !caID.MatchString(id) || len(pem) > maxCAPEM || !caPEM.MatchString(pem) {
			return "", errors.New("bad egress certificate")
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var block strings.Builder
	for _, id := range ids {
		block.WriteString(certs[id])
		if !strings.HasSuffix(certs[id], "\n") {
			block.WriteByte('\n')
		}
	}

	found := r.existingBundles()
	if len(found) == 0 {
		if len(ids) == 0 {
			return "/" + defaultCAs, nil
		}
		found = []string{defaultCAs}
		if err := os.MkdirAll(r.join(filepath.Dir(defaultCAs)), 0o755); err != nil {
			return "", err
		}
	}
	var errs []error
	for _, rel := range found {
		if err := r.writeBundle(rel, block.String()); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rel, err))
		}
	}
	return "/" + found[0], errors.Join(errs...)
}

// existingBundles lists the bundles present in the image, once per underlying file.
func (r Root) existingBundles() []string {
	var found []string
	seen := map[string]bool{}
	for _, rel := range caBundles {
		real, ok := r.resolve(rel)
		if !ok || seen[real] {
			continue
		}
		seen[real] = true
		found = append(found, rel)
	}
	return found
}

// resolve follows symlinks inside the root, so an absolute link in the image does not point at the host.
func (r Root) resolve(rel string) (string, bool) {
	p := "/" + rel
	for range 40 {
		fi, err := os.Lstat(r.join(p))
		if err != nil {
			return "", false
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			return p, fi.Mode().IsRegular()
		}
		target, err := os.Readlink(r.join(p))
		if err != nil {
			return "", false
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		p = filepath.Clean(target)
	}
	return "", false
}

func (r Root) writeBundle(rel, block string) error {
	real, ok := r.resolve(rel)
	if !ok {
		real = "/" + rel
	}
	path := r.join(real)
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	out := stripBlock(string(data), caBlockBegin, caBlockEnd)
	if block != "" {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += caBlockBegin + block + caBlockEnd
	}
	if out == string(data) {
		return nil
	}
	return writeFileAtomic(path, []byte(out), 0o644)
}

// stripBlock drops the marked block; a begin marker without its end drops the rest of the text.
func stripBlock(text, begin, end string) string {
	var keep strings.Builder
	inside := false
	for line := range strings.SplitAfterSeq(text, "\n") {
		switch {
		case line == begin || line == strings.TrimSuffix(begin, "\n"):
			inside = true
		case line == end || line == strings.TrimSuffix(end, "\n"):
			inside = false
		case !inside:
			keep.WriteString(line)
		}
	}
	return keep.String()
}
