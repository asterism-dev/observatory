package guestinit

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const debBundleAbs = "/" + debBundle

var withPath = []string{trustBlock[0], trustBlock[1], trustBlock[2], `PATH="/.observatory/bin:` + defaultPath + `"`, trustBlock[3], trustBlock[4]}

var trustBlock = []string{
	`CURL_CA_BUNDLE="` + debBundleAbs + `"`, `GRPC_DEFAULT_SSL_ROOTS_FILE_PATH="` + debBundleAbs + `"`, `NODE_EXTRA_CA_CERTS="` + debBundleAbs + `"`,
	`REQUESTS_CA_BUNDLE="` + debBundleAbs + `"`, `SSL_CERT_FILE="` + debBundleAbs + `"`,
}

func envRoot(t *testing.T) Root {
	t.Helper()
	r := Root{t.TempDir()}
	put(t, r, debBundle, "ROOTS\n", 0o644)
	return r
}

func vmmBlock(t *testing.T, environment string) []string {
	t.Helper()
	i, j := strings.Index(environment, "# BEGIN vmm\n"), strings.Index(environment, "# END vmm\n")
	if i < 0 && j < 0 {
		return nil
	}
	if i < 0 || j < i {
		t.Fatalf("broken managed block:\n%s", environment)
	}
	return strings.Split(strings.TrimSuffix(environment[i+len("# BEGIN vmm\n"):j], "\n"), "\n")
}

// sourcedEnv sources env.sh and reports A to E as a child process sees them.
func sourcedEnv(t *testing.T, r Root) string {
	t.Helper()
	out, err := exec.Command("sh", "-c", `. "$1"; exec sh -c 'printf "%s\0" "${A-unset}" "${B-unset}" "${C-unset}" "${D-unset}" "${E-unset}"'`, "sh", filepath.Join(r.Path, "etc/observatory/env.sh")).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestWriteEnvRoundTrip(t *testing.T) {
	r := envRoot(t)
	put(t, r, "etc/environment", "PATH=\"/usr/bin\"\n", 0o644)
	// pam_env cuts D at the # and splits E at its line buffer, so both stay out of /etc/environment.
	env := map[string]string{"A": "x\ny", "B": `it's $HOME \ "q" =`, "C": "", "D": "x#y", "E": strings.Repeat("a", 1001) + "X=injected"}
	if _, err := r.WriteEnv(env, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := sourcedEnv(t, r), "x\ny\x00"+env["B"]+"\x00\x00x#y\x00"+env["E"]+"\x00"; got != want {
		t.Fatalf("env.sh sets %q, want %q", got, want)
	}
	environment := get(t, r, "etc/environment")
	if !strings.HasPrefix(environment, "PATH=\"/usr/bin\"\n") {
		t.Fatalf("/etc/environment lost its own lines:\n%s", environment)
	}
	if got, want := vmmBlock(t, environment), append([]string{`B="` + env["B"] + `"`, `C=""`}, withPath...); !slices.Equal(got, want) {
		t.Fatalf("managed block = %q, want %q", got, want)
	}
	for _, p := range []string{"etc/observatory/env.sh", "etc/environment", "etc/profile.d/observatory.sh"} {
		if st := modeOf(t, r, p); st != 0o644 {
			t.Errorf("%s mode = %v", p, st)
		}
	}
	if got := get(t, r, "etc/profile.d/observatory.sh"); !strings.Contains(got, "/etc/observatory/env.sh") || !strings.Contains(got, "/.observatory/bin") {
		t.Fatalf("profile.d = %q", got)
	}

	if _, err := r.WriteEnv(nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := sourcedEnv(t, r); got != strings.Repeat("unset\x00", 5) {
		t.Fatalf("env.sh after second write sets %q", got)
	}
	if got, want := get(t, r, "etc/environment"), "PATH=\"/usr/bin\"\n# BEGIN vmm\n"+strings.Join(withPath, "\n")+"\n# END vmm\n"; got != want {
		t.Fatalf("/etc/environment after second write:\n%s", got)
	}
}

func TestWriteEnvTrustVars(t *testing.T) {
	r := envRoot(t)
	put(t, r, "lib/systemd/systemd", "", 0o755)
	merged, err := r.WriteEnv(map[string]string{"SSL_CERT_FILE": "/x"}, []string{"PATH=/opt/bin:/usr/bin", "NODE_EXTRA_CA_CERTS=/img"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"SSL_CERT_FILE": "/x", "NODE_EXTRA_CA_CERTS": "/img", "REQUESTS_CA_BUNDLE": debBundleAbs, "CURL_CA_BUNDLE": debBundleAbs, "PATH": "/.observatory/bin:/opt/bin:/usr/bin",
	}
	for k, v := range want {
		if merged[k] != v {
			t.Errorf("merged[%s] = %q, want %q", k, merged[k], v)
		}
	}
	if got := get(t, r, "etc/observatory/env.sh"); strings.Count(got, "SSL_CERT_FILE=") != 1 || !strings.Contains(got, "export SSL_CERT_FILE='/x'\n") {
		t.Fatalf("env.sh = %q", got)
	}
	for _, d := range []string{"system", "user"} {
		got := get(t, r, "etc/systemd/"+d+".conf.d/50-observatory.conf")
		if !strings.HasPrefix(got, "[Manager]\nDefaultEnvironment=") {
			t.Fatalf("%s.conf.d = %q", d, got)
		}
		for _, w := range []string{`"SSL_CERT_FILE=/x"`, `"REQUESTS_CA_BUNDLE=` + debBundleAbs + `"`, `"CURL_CA_BUNDLE=`, `"NODE_EXTRA_CA_CERTS=/img"`, `"PATH=/.observatory/bin:/opt/bin:/usr/bin"`} {
			if !strings.Contains(got, w) {
				t.Errorf("%s.conf.d misses %s: %q", d, w, got)
			}
		}
	}
}

func TestWriteEnvGRPCRoots(t *testing.T) {
	r := envRoot(t)
	put(t, r, "lib/systemd/systemd", "", 0o755)
	merged, err := r.WriteEnv(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if merged["GRPC_DEFAULT_SSL_ROOTS_FILE_PATH"] != debBundleAbs {
		t.Fatalf("merged = %v", merged)
	}
	if got := get(t, r, "etc/systemd/system.conf.d/50-observatory.conf"); !strings.Contains(got, `"GRPC_DEFAULT_SSL_ROOTS_FILE_PATH=`+debBundleAbs+`"`) {
		t.Fatalf("system.conf.d = %q", got)
	}
}

func TestWriteEnvNoSystemdNoDropIn(t *testing.T) {
	r := envRoot(t)
	if _, err := r.WriteEnv(nil, nil); err != nil {
		t.Fatal(err)
	}
	if exists(r, "etc/systemd/system.conf.d/50-observatory.conf") {
		t.Fatal("drop-in written without systemd")
	}
}

func TestWriteEnvDefaultPathHasPlatformDir(t *testing.T) {
	r := envRoot(t)
	put(t, r, "lib/systemd/systemd", "", 0o755)
	if _, err := r.WriteEnv(nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := get(t, r, "etc/systemd/system.conf.d/50-observatory.conf"); !strings.Contains(got, `"PATH=/.observatory/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"`) {
		t.Fatalf("drop-in = %q", got)
	}
}

func TestWriteEnvSeedWinsOverImage(t *testing.T) {
	r := envRoot(t)
	merged, err := r.WriteEnv(map[string]string{"A": "seed"}, []string{"A=image", "B=a=b", "NOEQUALS"})
	if err == nil {
		t.Fatal("entry without = accepted")
	}
	if merged["A"] != "seed" || merged["B"] != "a=b" {
		t.Fatalf("merged = %v", merged)
	}
	if _, ok := merged["NOEQUALS"]; ok {
		t.Fatal("image entry without = kept")
	}
	if got := get(t, r, "etc/observatory/env.sh"); strings.Contains(got, "image") {
		t.Fatalf("env.sh = %q", got)
	}
}

func TestWriteEnvRejectsBadNames(t *testing.T) {
	r := envRoot(t)
	merged, err := r.WriteEnv(map[string]string{"A": "1", "x; rm -rf /": "1", "1X": "1", "B\x00": "1", "OK": "a\x00b"}, []string{"a-b=2", "C=3"})
	if err == nil {
		t.Fatal("bad names accepted")
	}
	for _, leak := range []string{"rm -rf", "a\x00b"} {
		if strings.Contains(get(t, r, "etc/observatory/env.sh"), leak) {
			t.Fatalf("env.sh contains %q", leak)
		}
	}
	if merged["A"] != "1" || merged["C"] != "3" || len(merged) != 3+len(trustBlock) {
		t.Fatalf("merged = %v", merged)
	}
	if strings.Contains(err.Error(), "=") {
		t.Fatalf("error leaks values: %v", err)
	}
}

func modeOf(t *testing.T, r Root, rel string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(filepath.Join(r.Path, rel))
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestWriteEnvPlatformDirFirstOnEveryPath(t *testing.T) {
	for name, image := range map[string][]string{"with image PATH": {"PATH=/opt/bin:/usr/bin"}, "without image PATH": nil} {
		r := envRoot(t)
		merged, err := r.WriteEnv(nil, image)
		if err != nil {
			t.Fatal(err)
		}
		want := "/.observatory/bin:" + defaultPath
		if image != nil {
			want = "/.observatory/bin:/opt/bin:/usr/bin"
		}
		if merged["PATH"] != want {
			t.Errorf("%s: merged PATH = %q", name, merged["PATH"])
		}
		if !strings.Contains(get(t, r, "etc/observatory/env.sh"), "export PATH='"+want+"'\n") {
			t.Errorf("%s: env.sh lacks PATH", name)
		}
		if !slices.Contains(vmmBlock(t, get(t, r, "etc/environment")), `PATH="`+want+`"`) {
			t.Errorf("%s: /etc/environment lacks PATH", name)
		}
	}
	r := envRoot(t)
	merged, _ := r.WriteEnv(map[string]string{"PATH": "/seed"}, []string{"PATH=/img"})
	if merged["PATH"] != "/.observatory/bin:/seed" {
		t.Errorf("seed PATH = %q", merged["PATH"])
	}
}

func TestWriteEnvErrorDoesNotLeakImageEntry(t *testing.T) {
	_, err := envRoot(t).WriteEnv(nil, []string{"sekrit-value"})
	if err == nil || strings.Contains(err.Error(), "sekrit") {
		t.Fatalf("err = %v", err)
	}
}
