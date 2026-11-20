package guestinit

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
)

const (
	platformBin       = "/.observatory/bin"
	defaultPath       = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	maxEnvironmentVal = 1000
)

var (
	envName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	trustVars = []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "GRPC_DEFAULT_SSL_ROOTS_FILE_PATH"}
)

// WriteEnv merges trust vars, image env and seed env (later wins) into the files shells, PAM and systemd read; bad names are skipped and reported in the error.
func (r Root) WriteEnv(seed map[string]string, image []string) (map[string]string, error) {
	var bad []string
	env := map[string]string{}
	if found := r.existingBundles(); len(found) > 0 {
		for _, n := range trustVars {
			env[n] = "/" + found[0]
		}
	}
	set := func(n, v string) {
		if !envName.MatchString(n) || strings.ContainsRune(v, 0) {
			bad = append(bad, fmt.Sprintf("%q", n))
			return
		}
		env[n] = v
	}
	for i, kv := range image {
		n, v, ok := strings.Cut(kv, "=")
		if !ok {
			bad = append(bad, fmt.Sprintf("image entry #%d", i))
			continue
		}
		set(n, v)
	}
	for n, v := range seed {
		set(n, v)
	}
	slices.Sort(bad)
	path := env["PATH"]
	if path == "" {
		path = defaultPath
	}
	if !slices.Contains(strings.Split(path, ":"), platformBin) {
		path = platformBin + ":" + path
	}
	env["PATH"] = path

	names := slices.Sorted(maps.Keys(env))
	var envSh, block strings.Builder
	for _, n := range names {
		fmt.Fprintf(&envSh, "export %s=%s\n", n, shellQuote(env[n]))
		// pam_env cuts values at "#" and splits long lines into new KEY=VAL lines.
		if v := env[n]; !strings.ContainsAny(v, "\r\n#") && len(v) <= maxEnvironmentVal {
			fmt.Fprintf(&block, "%s=\"%s\"\n", n, v)
		}
	}
	if err := writeFileAtomic(r.join("etc/observatory/env.sh"), []byte(envSh.String()), 0o644); err != nil {
		return nil, err
	}
	if err := r.writeEnvironmentBlock(block.String()); err != nil {
		return nil, err
	}
	profile := "if [ -r /etc/observatory/env.sh ]; then . /etc/observatory/env.sh; fi\n" +
		"case \":$PATH:\" in *:" + platformBin + ":*) ;; *) PATH=" + platformBin + ":$PATH; export PATH ;; esac\n"
	if err := writeFileAtomic(r.join("etc/profile.d/observatory.sh"), []byte(profile), 0o644); err != nil {
		return nil, err
	}
	if r.executable("/lib/systemd/systemd") {
		if err := r.writeSystemdEnv(env); err != nil {
			return nil, err
		}
	}
	if len(bad) > 0 {
		return env, fmt.Errorf("skipped invalid environment entries: %s", strings.Join(bad, ", "))
	}
	return env, nil
}

func (r Root) writeEnvironmentBlock(block string) error {
	path := r.join("etc/environment")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	out := stripBlock(string(data), "# BEGIN vmm\n", "# END vmm\n")
	if block != "" {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += "# BEGIN vmm\n" + block + "# END vmm\n"
	}
	return writeFileAtomic(path, []byte(out), 0o644)
}

func (r Root) writeSystemdEnv(env map[string]string) error {
	vars := map[string]string{"PATH": env["PATH"]}
	for _, n := range trustVars {
		if v, ok := env[n]; ok {
			vars[n] = v
		}
	}
	var line strings.Builder
	for _, n := range slices.Sorted(maps.Keys(vars)) {
		fmt.Fprintf(&line, " %q", n+"="+vars[n])
	}
	conf := "[Manager]\nDefaultEnvironment=" + strings.TrimPrefix(line.String(), " ") + "\n"
	for _, d := range []string{"system", "user"} {
		if err := writeFileAtomic(r.join("etc/systemd/"+d+".conf.d/50-observatory.conf"), []byte(conf), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func envList(env map[string]string) []string {
	var out []string
	for _, n := range slices.Sorted(maps.Keys(env)) {
		out = append(out, n+"="+env[n])
	}
	return out
}
