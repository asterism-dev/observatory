// Package buildagent runs one image build inside a builder VM: it fetches the recipe from the hub, builds and pushes it with BuildKit, and streams the masked log.
package buildagent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	logInterval = time.Second
	readyWait   = 60 * time.Second
	postTries   = 5
)

// secretID is both the BuildKit secret id and its file name, so it must not carry commas or path separators.
var secretID = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// Agent holds one build's settings, all from the builder VM's env.
type Agent struct {
	Hub          string
	BuildID      string
	Secret       string
	ImageID      string
	Secrets      []string
	Integrations []string
	// Dir holds the recipe, the secret files and the docker config; it should be a tmpfs.
	Dir string

	client *http.Client
}

// FromEnv reads OBSERVATORY_BUILD_URL, _ID, _SECRET, _IMAGE and the comma-separated reference lists OBSERVATORY_BUILD_SECRETS and _INTEGRATIONS.
func FromEnv(dir string) (*Agent, error) {
	a := &Agent{
		Hub:          strings.TrimRight(os.Getenv("OBSERVATORY_BUILD_URL"), "/"),
		BuildID:      os.Getenv("OBSERVATORY_BUILD_ID"),
		Secret:       os.Getenv("OBSERVATORY_BUILD_SECRET"),
		ImageID:      os.Getenv("OBSERVATORY_BUILD_IMAGE"),
		Secrets:      list(os.Getenv("OBSERVATORY_BUILD_SECRETS")),
		Integrations: list(os.Getenv("OBSERVATORY_BUILD_INTEGRATIONS")),
		Dir:          dir,
		client:       &http.Client{Timeout: 5 * time.Minute},
	}
	if a.Hub == "" || a.BuildID == "" || a.Secret == "" || a.ImageID == "" {
		return nil, errors.New("OBSERVATORY_BUILD_URL, OBSERVATORY_BUILD_ID, OBSERVATORY_BUILD_SECRET and OBSERVATORY_BUILD_IMAGE are required")
	}
	return a, nil
}

func list(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// Run builds and pushes the image and reports the digest or the error to the hub; it returns an error only when the result could not be reported.
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := os.MkdirAll(a.Dir, 0o700); err != nil {
		return a.postResult(ctx, "", err)
	}
	secrets, values, err := a.writeSecrets(ctx)
	log := NewMasker(append(values, []byte(a.Secret)))
	shipped := a.shipLog(ctx, log)
	var digest string
	if err == nil {
		digest, err = a.build(ctx, log, secrets)
	}
	if err != nil {
		fmt.Fprintf(log, "build failed: %v\n", err)
	}
	log.Close()
	cancel()
	<-shipped
	return a.postResult(context.Background(), digest, err)
}

// writeSecrets writes one file per reference and returns the --secret flags and the values to mask.
func (a *Agent) writeSecrets(ctx context.Context) ([]string, [][]byte, error) {
	dir := filepath.Join(a.Dir, "secrets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	var flags []string
	var values [][]byte
	write := func(id, value string) error {
		if !secretID.MatchString(id) {
			return fmt.Errorf("invalid secret id %q", id)
		}
		file := filepath.Join(dir, id)
		if err := os.WriteFile(file, []byte(value), 0o600); err != nil {
			return err
		}
		flags = append(flags, "--secret", "id="+id+",src="+file)
		values = append(values, []byte(value))
		return nil
	}
	for _, name := range a.Secrets {
		v, ok := os.LookupEnv(name)
		if !ok {
			return nil, nil, fmt.Errorf("secret %s is not set", name)
		}
		if err := write(name, v); err != nil {
			return nil, nil, err
		}
	}
	for _, kind := range a.Integrations {
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, "vmm-token", kind)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, nil, fmt.Errorf("integration %s: vmm-token: %v %s", kind, err, strings.TrimSpace(stderr.String()))
		}
		if err := write(kind, strings.TrimSpace(string(out))); err != nil {
			return nil, nil, err
		}
	}
	return flags, values, nil
}

func (a *Agent) build(ctx context.Context, log io.Writer, secretFlags []string) (string, error) {
	hub, err := url.Parse(a.Hub)
	if err != nil {
		return "", err
	}
	if err := a.startBuildkitd(ctx, hub); err != nil {
		return "", err
	}
	fmt.Fprintln(log, "fetching the recipe")
	recipe := filepath.Join(a.Dir, "recipe")
	if err := a.fetchRecipe(ctx, recipe); err != nil {
		return "", fmt.Errorf("recipe: %w", err)
	}
	dockerConfig := filepath.Join(a.Dir, "docker")
	if err := a.writeDockerConfig(dockerConfig, hub.Host); err != nil {
		return "", err
	}
	meta := filepath.Join(a.Dir, "metadata.json")
	args := []string{"build", "--progress", "plain",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + recipe, "--local", "dockerfile=" + recipe,
		// No attest: options at all: any attest:provenance value, even "false", turns provenance on and pushes an index instead of a plain manifest.
		"--opt", "platform=linux/amd64",
	}
	args = append(args, secretFlags...)
	args = append(args,
		"--output", "type=image,name="+hub.Host+"/i/"+a.ImageID+":build-"+a.BuildID+",push=true",
		"--metadata-file", meta)
	cmd := exec.CommandContext(ctx, "buildctl", args...)
	cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+dockerConfig)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("buildctl: %w", err)
	}
	raw, err := os.ReadFile(meta)
	if err != nil {
		return "", err
	}
	var m struct {
		Digest string `json:"containerimage.digest"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Digest == "" {
		return "", fmt.Errorf("no image digest in the build metadata: %s", raw)
	}
	return m.Digest, nil
}

// startBuildkitd starts the daemon for the lifetime of ctx and waits until buildctl reaches it.
func (a *Agent) startBuildkitd(ctx context.Context, hub *url.URL) error {
	conf := ""
	// Builder VMs may talk to the hub's plain internal URL, which serves the registry too.
	if hub.Scheme == "http" {
		conf = fmt.Sprintf("[registry.%q]\n  http = true\n", hub.Host)
	}
	confFile := filepath.Join(a.Dir, "buildkitd.toml")
	if err := os.WriteFile(confFile, []byte(conf), 0o600); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "buildkitd", "--config", confFile)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("buildkitd: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.Now().Add(readyWait)
	for {
		if exec.CommandContext(ctx, "buildctl", "debug", "workers").Run() == nil {
			return nil
		}
		select {
		case err := <-exited:
			return fmt.Errorf("buildkitd exited: %v", err)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return errors.New("buildkitd did not start within a minute")
		}
	}
}

// writeDockerConfig lets BuildKit get registry tokens from the hub with the build credential, renewing them itself as they expire.
func (a *Agent) writeDockerConfig(dir, host string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	auth := base64.StdEncoding.EncodeToString([]byte("observatory:" + a.Secret))
	raw, err := json.Marshal(map[string]any{"auths": map[string]any{host: map[string]string{"auth": auth}}})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), raw, 0o600)
}

func (a *Agent) fetchRecipe(ctx context.Context, dir string) error {
	req, err := a.request(ctx, http.MethodGet, "context", nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub answered %s", resp.Status)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, 0o755)
		case tar.TypeReg:
			err = extractFile(root, name, hdr.FileInfo().Mode().Perm(), tr)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", hdr.Name, err)
		}
	}
}

func extractFile(root *os.Root, name string, mode os.FileMode, r io.Reader) error {
	if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// shipLog posts the masked log every second until ctx ends, then sends the rest; text that fails to post is kept for the next try.
func (a *Agent) shipLog(ctx context.Context, log *Masker) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var pending []byte
		send := func(ctx context.Context) error {
			pending = append(pending, log.Take()...)
			if len(pending) == 0 {
				return nil
			}
			if err := a.post(ctx, "log", "text/plain; charset=utf-8", pending); err != nil {
				return err
			}
			pending = nil
			return nil
		}
		tick := time.NewTicker(logInterval)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				_ = send(ctx)
			case <-ctx.Done():
				if err := retry(func() error { return send(context.Background()) }); err != nil {
					fmt.Fprintln(os.Stderr, "build-agent: log:", err)
				}
				return
			}
		}
	}()
	return done
}

func (a *Agent) postResult(ctx context.Context, digest string, buildErr error) error {
	result := map[string]string{"digest": digest}
	if buildErr != nil {
		result = map[string]string{"error": buildErr.Error()}
	}
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return retry(func() error { return a.post(ctx, "result", "application/json", body) })
}

func retry(f func() error) error {
	var err error
	for i := range postTries {
		if i > 0 {
			time.Sleep(2 * time.Second)
		}
		if err = f(); err == nil {
			return nil
		}
	}
	return err
}

func (a *Agent) post(ctx context.Context, endpoint, contentType string, body []byte) error {
	req, err := a.request(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: hub answered %s", endpoint, resp.Status)
	}
	return nil
}

func (a *Agent) request(ctx context.Context, method, endpoint string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.Hub+"/api/builds/"+url.PathEscape(a.BuildID)+"/"+endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Secret)
	return req, nil
}
