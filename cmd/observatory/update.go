package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/asterism-dev/observatory/internal/release"
	"github.com/asterism-dev/observatory/internal/version"
)

const (
	downloadURL      = "https://github.com/asterism-dev/observatory/releases/download"
	updateCheckFile  = "update-check.json"
	updateCheckEvery = 24 * time.Hour
)

type updateCheck struct {
	CheckedAt time.Time `json:"checkedAt"`
	Latest    string    `json:"latest,omitempty"`
}

// updateHint prints a stderr hint when a newer release exists; it asks GitHub at most once a day and never fails.
func (c *cli) updateHint() {
	if !release.Enabled(c.getenv) || c.cfgDir == "" {
		return
	}
	path := filepath.Join(c.cfgDir, updateCheckFile)
	var uc updateCheck
	if b, err := os.ReadFile(path); err != nil || json.Unmarshal(b, &uc) != nil || c.now().Sub(uc.CheckedAt) >= updateCheckEvery {
		ctx, cancel := context.WithTimeout(c.ctx, 3*time.Second)
		latest, _ := release.Latest(ctx, c.http, c.latestURL)
		cancel()
		// Written on failure too, so an outage costs one request a day.
		uc = updateCheck{CheckedAt: c.now(), Latest: latest}
		if b, err := json.Marshal(uc); err == nil {
			_ = os.WriteFile(path, b, 0o600)
		}
	}
	if version.Older(version.Version, uc.Latest) {
		fmt.Fprintf(c.stderr, "observatory: version %s is available (running %s), run observatory update\n", uc.Latest, version.Version)
	}
}

func cmdUpdate(c *cli, args []string) error {
	if _, err := c.parse(c.flags("update"), args, 0, 0); err != nil {
		return err
	}
	if !version.Released(version.Version) {
		return errors.New("dev build, install a release first")
	}
	latest, err := release.Latest(c.ctx, c.http, c.latestURL)
	if err != nil {
		return err
	}
	if !version.Older(version.Version, latest) {
		fmt.Fprintf(c.stdout, "already up to date (%s)\n", version.Version)
		return nil
	}
	file := fmt.Sprintf("observatory_%s_%s_%s.tar.gz", latest, runtime.GOOS, runtime.GOARCH)
	sums, err := c.download(latest + "/SHA256SUMS")
	if err != nil {
		return err
	}
	want, err := checksumFor(sums, file)
	if err != nil {
		return err
	}
	tgz, err := c.download(latest + "/" + file)
	if err != nil {
		return err
	}
	if got := sha256.Sum256(tgz); hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("checksum mismatch for %s", file)
	}
	bin, err := extractBinary(tgz, "observatory")
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	exe, err := c.executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return err
	}
	if err := replaceFile(exe, bin); err != nil {
		return fmt.Errorf("cannot replace %s: %w (try sudo observatory update)", exe, err)
	}
	fmt.Fprintf(c.stdout, "updated %s → %s\n", version.Version, latest)
	return nil
}

func (c *cli) download(path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, c.downloadURL+"/"+path, nil)
	if err != nil {
		return nil, err
	}
	// Release tarballs are a few MB; the default 30 s client timeout is too short on slow links.
	client := *c.http
	client.Timeout = 5 * time.Minute
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", path, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func checksumFor(sums []byte, file string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if sum, name, ok := strings.Cut(sc.Text(), "  "); ok && name == file {
			return sum, nil
		}
	}
	return "", fmt.Errorf("%s is missing from SHA256SUMS", file)
}

func extractBinary(tgz []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("no %s in archive", name)
		}
		if err != nil {
			return nil, err
		}
		if h.Name == name {
			return io.ReadAll(tr)
		}
	}
}

// replaceFile writes next to path and renames over it, so a failure leaves the old binary intact.
func replaceFile(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".observatory-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o755)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
