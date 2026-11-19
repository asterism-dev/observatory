package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const credHelperName = "docker-credential-observatory"

func isCredHelper(argv0 string) bool { return filepath.Base(argv0) == credHelperName }

// runCredHelper speaks docker's credential helper protocol; store and erase do nothing as credentials come from the CLI session.
func runCredHelper(c *cli, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "usage: "+credHelperName+" get|store|erase")
		return 2
	}
	switch args[0] {
	case "store", "erase":
		return 0
	case "get":
		if err := c.credGet(); err != nil {
			fmt.Fprintf(c.stderr, "observatory: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(c.stderr, "observatory: unsupported command %q\n", args[0])
	return 1
}

func (c *cli) credGet() error {
	dir, err := configDir(c.getenv)
	if err != nil {
		return err
	}
	c.cfgDir = dir
	if c.cfg, err = loadConfig(dir); err != nil {
		return err
	}
	in, err := io.ReadAll(io.LimitReader(c.stdin, 4096))
	if err != nil {
		return err
	}
	server := strings.TrimSpace(string(in))
	host, err := c.registryHost()
	if err != nil {
		return err
	}
	if bare := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(server, "https://"), "http://"), "/"); bare != host {
		return fmt.Errorf("%s is not the hub registry %s", server, host)
	}
	var cred struct{ Username, Secret string }
	if err := c.call(http.MethodPost, "/api/registry/credentials", nil, &cred); err != nil {
		return err
	}
	return json.NewEncoder(c.stdout).Encode(map[string]string{"ServerURL": server, "Username": cred.Username, "Secret": cred.Secret})
}

func (c *cli) dockerConfigPath() (string, error) {
	if d := c.getenv("DOCKER_CONFIG"); d != "" {
		return filepath.Join(d, "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".docker", "config.json"), nil
}

func cmdImageLogin(c *cli, args []string) error {
	if _, err := c.parse(c.flags("image login"), args, 0, 0); err != nil {
		return err
	}
	host, err := c.registryHost()
	if err != nil {
		return err
	}
	cfgPath, err := c.dockerConfigPath()
	if err != nil {
		return err
	}
	if err := setCredHelper(cfgPath, host); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "Set credHelpers for %s to observatory in %s.\n", host, cfgPath)
	link, err := c.linkCredHelper()
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "Linked %s.\n", link)
	if !pathHas(c.getenv("PATH"), filepath.Dir(link)) {
		fmt.Fprintf(c.stdout, "Add %s to your PATH so docker finds it.\n", filepath.Dir(link))
	}
	return nil
}

func pathHas(pathEnv, dir string) bool {
	for _, p := range filepath.SplitList(pathEnv) {
		if p == dir {
			return true
		}
	}
	return false
}

// setCredHelper keeps every other key of docker's config as it is.
func setCredHelper(cfgPath, host string) error {
	cfg := map[string]json.RawMessage{}
	mode := os.FileMode(0o600)
	b, err := os.ReadFile(cfgPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(b, &cfg); err != nil {
			return fmt.Errorf("%s is not valid JSON: %w", cfgPath, err)
		}
		if st, err := os.Stat(cfgPath); err == nil {
			mode = st.Mode().Perm()
		}
	}
	helpers := map[string]json.RawMessage{}
	if raw, ok := cfg["credHelpers"]; ok {
		if err := json.Unmarshal(raw, &helpers); err != nil {
			return fmt.Errorf("credHelpers in %s is not an object: %w", cfgPath, err)
		}
	}
	helpers[host] = json.RawMessage(`"observatory"`)
	cfg["credHelpers"], _ = json.Marshal(helpers)
	out, err := json.MarshalIndent(cfg, "", "\t")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(cfgPath, append(out, '\n'), mode)
}

// linkCredHelper links the CLI binary as the helper next to itself, or in ~/.local/bin when that directory is not writable.
func (c *cli) linkCredHelper() (string, error) {
	exe, err := c.executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	home, herr := os.UserHomeDir()
	dirs := []string{filepath.Dir(exe)}
	if herr == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"))
	}
	var errs []error
	for _, dir := range dirs {
		link := filepath.Join(dir, credHelperName)
		if err := replaceSymlink(exe, link); err != nil {
			errs = append(errs, err)
			continue
		}
		return link, nil
	}
	return "", errors.Join(errs...)
}

func replaceSymlink(target, link string) error {
	if cur, err := os.Readlink(link); err == nil && cur == target {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Symlink(target, link)
}
