package guestinit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	devName = "dev"
	devID   = 1000
	devHome = "/home/dev"
)

// EnsureDev makes uid 1000 the login user "dev", renaming an existing account instead of adding a second one.
func (r Root) EnsureDev() error {
	oldName, oldHome, err := r.editPasswd()
	if err != nil {
		return err
	}
	if err := r.editGroup(oldName); err != nil {
		return err
	}
	if err := r.editShadow(oldName); err != nil {
		return err
	}
	if err := r.ensureHome(oldHome); err != nil {
		return err
	}
	if _, err := os.Lstat(r.join("usr/bin/sudo")); err == nil {
		dir := r.join("etc/sudoers.d")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		return writeFileAtomic(filepath.Join(dir, "observatory-dev"), []byte("dev ALL=(ALL) NOPASSWD:ALL\n"), 0o440)
	}
	return nil
}

// DevShell is the account's current shell if usable in the image, else /bin/sh, else /bin/false.
func (r Root) DevShell() string {
	if fields, ok := r.devPasswdFields(); ok && r.executable(fields[6]) {
		return fields[6]
	}
	if r.executable("/bin/sh") {
		return "/bin/sh"
	}
	return "/bin/false"
}

func (r Root) join(rel string) string { return filepath.Join(r.Path, rel) }

func (r Root) devPasswdFields() ([]string, bool) {
	data, err := os.ReadFile(r.join("etc/passwd"))
	if err != nil {
		return nil, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Split(line, ":"); len(f) == 7 && f[2] == "1000" {
			return f, true
		}
	}
	return nil, false
}

// executable resolves symlinks inside the root, not on the host.
func (r Root) executable(p string) bool {
	if p == "" {
		return false
	}
	for range 40 {
		fi, err := os.Lstat(r.join(p))
		if err != nil {
			return false
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			return fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
		}
		target, err := os.Readlink(r.join(p))
		if err != nil {
			return false
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		p = filepath.Clean(target)
	}
	return false
}

// editPasswd returns the previous name and home of the uid 1000 account ("" if there was none).
func (r Root) editPasswd() (oldName, oldHome string, err error) {
	shell := r.DevShell()
	path := r.join("etc/passwd")
	lines, err := readLines(path)
	if err != nil {
		return "", "", err
	}
	found := false
	for i, line := range lines {
		f := strings.Split(line, ":")
		if len(f) != 7 {
			continue
		}
		if f[2] == "1000" {
			oldName, oldHome = f[0], f[5]
			f[0], f[5], f[6] = devName, devHome, shell
			lines[i] = strings.Join(f, ":")
			found = true
		} else if f[0] == devName {
			return "", "", fmt.Errorf("account %q exists with uid %s", devName, f[2])
		}
	}
	if !found {
		lines = append(lines, fmt.Sprintf("dev:x:%d:%d::%s:%s", devID, devID, devHome, shell))
	}
	return oldName, oldHome, writeLines(path, lines, 0o644)
}

func (r Root) editGroup(oldName string) error {
	path := r.join("etc/group")
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	found := false
	for i, line := range lines {
		f := strings.Split(line, ":")
		if len(f) != 4 {
			continue
		}
		if f[2] == "1000" {
			f[0] = devName
			found = true
		} else if f[0] == devName {
			return fmt.Errorf("group %q exists with gid %s", devName, f[2])
		}
		if oldName != "" && oldName != devName {
			members := strings.Split(f[3], ",")
			for j, m := range members {
				if m == oldName {
					members[j] = devName
				}
			}
			f[3] = strings.Join(members, ",")
		}
		lines[i] = strings.Join(f, ":")
	}
	if !found {
		lines = append(lines, fmt.Sprintf("dev:x:%d:", devID))
	}
	return writeLines(path, lines, 0o644)
}

func (r Root) editShadow(oldName string) error {
	path := r.join("etc/shadow")
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	found := false
	for i, line := range lines {
		name, rest, ok := strings.Cut(line, ":")
		if ok && (name == devName || (oldName != "" && name == oldName)) {
			lines[i] = devName + ":" + rest
			found = true
		}
	}
	if !found {
		lines = append(lines, "dev:*:19000:0:99999:7:::")
	}
	return writeLines(path, lines, 0o600)
}

func (r Root) ensureHome(oldHome string) error {
	home := r.join(devHome)
	if oldHome != "" && filepath.Clean(oldHome) != devHome {
		old := r.join(oldHome)
		_, errOld := os.Lstat(old)
		_, errNew := os.Lstat(home)
		if errOld == nil && errors.Is(errNew, fs.ErrNotExist) {
			if err := os.Rename(old, home); err != nil {
				return err
			}
		}
	}
	if err := os.MkdirAll(home, 0o750); err != nil {
		return err
	}
	if err := os.Chmod(home, 0o750); err != nil {
		return err
	}
	return chown(home, devID, devID)
}

func chown(path string, uid, gid int) error {
	err := os.Lchown(path, uid, gid)
	if errors.Is(err, fs.ErrPermission) && os.Geteuid() != 0 {
		return nil
	}
	return err
}

func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"), nil
}

func writeLines(path string, lines []string, defaultMode os.FileMode) error {
	return writeFileAtomic(path, []byte(strings.Join(lines, "\n")+"\n"), defaultMode)
}

// writeFileAtomic keeps the mode and owner of an existing file.
func writeFileAtomic(path string, data []byte, defaultMode os.FileMode) error {
	mode, uid, gid := defaultMode, 0, 0
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	if err := chown(tmp.Name(), uid, gid); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
