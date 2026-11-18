package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func connectCommands() []command {
	return []command{
		{"ssh", "<vm> [--user ubuntu] [-- args...]", "ssh into a VM", cmdSSH},
		{"desktop", "<vm> [--user ubuntu]", "start the VM desktop and attach with xpra", cmdDesktop},
		{"ssh-key list", "", "list your SSH keys", cmdSSHKeyList},
		{"ssh-key add", "[FILE] [--name N]", "add a public key (default ~/.ssh/id_ed25519.pub)", cmdSSHKeyAdd},
		{"ssh-key delete", "<key>", "delete an SSH key", cmdSSHKeyDelete},
	}
}

func (c *cli) sshHost(key string) (vmJSON, string, error) {
	v, err := c.findVM(key)
	if err != nil {
		return v, "", err
	}
	host := v.DNSName
	if host == "" {
		host = v.MgmtIP
	}
	if host == "" {
		return v, "", &exitError{1, "vm has no management address yet"}
	}
	switch v.State {
	case "stopped", "paused", "hibernated":
		return v, "", &exitError{1, fmt.Sprintf("vm %s is %s; run observatory vm start %s", v.Name, v.State, v.Name)}
	}
	return v, host, nil
}

func (c *cli) warnNoDevice() {
	me, err := c.me(nil)
	if err != nil {
		return
	}
	path, err := c.orgPath("/devices")
	if err != nil {
		return
	}
	var devices []struct {
		Owner ref `json:"owner"`
	}
	if err := c.call(http.MethodGet, path, nil, &devices); err != nil {
		return
	}
	for _, d := range devices {
		if d.Owner.ID == me.User.ID {
			return
		}
	}
	fmt.Fprintf(c.stderr, "observatory: no device in org %s; run observatory device create <name> and import it\n", c.org.Name)
}

func (c *cli) sshTarget(cmdPath string, args []string, max int) (target string, rest []string, err error) {
	fs := c.flags(cmdPath)
	user := fs.String("user", "ubuntu", "login `user`")
	pos, err := c.parse(fs, args, 1, max)
	if err != nil {
		return "", nil, err
	}
	_, host, err := c.sshHost(pos[0])
	if err != nil {
		return "", nil, err
	}
	c.warnNoDevice()
	return *user + "@" + host, pos[1:], nil
}

func cmdSSH(c *cli, args []string) error {
	target, rest, err := c.sshTarget("ssh", args, -1)
	if err != nil {
		return err
	}
	path, err := c.lookPath("ssh")
	if err != nil {
		return &exitError{1, "ssh not found"}
	}
	return c.exec(path, append([]string{"ssh", target}, rest...), os.Environ())
}

func cmdDesktop(c *cli, args []string) error {
	target, _, err := c.sshTarget("desktop", args, 1)
	if err != nil {
		return err
	}
	sshPath, err := c.lookPath("ssh")
	if err != nil {
		return &exitError{1, "ssh not found"}
	}
	xpra, err := c.lookPath("xpra")
	if err != nil {
		const app = "/Applications/Xpra.app/Contents/MacOS/Xpra"
		if _, serr := os.Stat(app); serr != nil {
			return &exitError{1, "xpra not found; install the xpra 6 client, see docs/guest.md"}
		}
		xpra = app
	}
	if err := c.runProg(sshPath, []string{"ssh", target, "vmm-desktop", "start"}); err != nil {
		return err
	}
	return c.exec(xpra, []string{xpra, "attach", "ssh://" + target + "/100"}, os.Environ())
}

type sshKey struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"createdAt"`
}

func cmdSSHKeyList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("ssh-key list"), args, 0, 0); err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, "/api/me/ssh-keys", nil, &raw); err != nil {
		return err
	}
	var keys []sshKey
	if err := json.Unmarshal(raw, &keys); err != nil {
		return err
	}
	rows := make([][]string, len(keys))
	for i, k := range keys {
		rows[i] = []string{k.ID, k.Name, k.Fingerprint, ago(c.now(), k.CreatedAt)}
	}
	return c.emit(raw, []string{"id", "name", "fingerprint", "added"}, rows)
}

func cmdSSHKeyAdd(c *cli, args []string) error {
	fs := c.flags("ssh-key add")
	name := fs.String("name", "", "key `name` (default: the key's comment or file name)")
	pos, err := c.parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	file, err := defaultPublicKey(pos)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if strings.Contains(string(b), "PRIVATE KEY") {
		return usagef("%s contains a private key; pass the .pub file", file)
	}
	key := strings.TrimSpace(string(b))
	if *name == "" {
		*name = filepath.Base(file)
		if f := strings.Fields(key); len(f) >= 3 {
			*name = f[2]
		}
	}
	var raw json.RawMessage
	body := map[string]string{"name": *name, "publicKey": key}
	if err := c.call(http.MethodPost, "/api/me/ssh-keys", body, &raw); err != nil {
		return err
	}
	return c.done(raw, "added ssh key %s", *name)
}

func defaultPublicKey(pos []string) (string, error) {
	if len(pos) == 1 {
		return pos[0], nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	for _, n := range []string{"id_ed25519.pub", "id_ecdsa.pub", "id_rsa.pub"} {
		p := filepath.Join(home, ".ssh", n)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", usagef("no public key in ~/.ssh; pass a FILE")
}

func cmdSSHKeyDelete(c *cli, args []string) error {
	pos, err := c.parse(c.flags("ssh-key delete"), args, 1, 1)
	if err != nil {
		return err
	}
	var keys []sshKey
	if err := c.call(http.MethodGet, "/api/me/ssh-keys", nil, &keys); err != nil {
		return err
	}
	refs := make([]ref, len(keys))
	for i, k := range keys {
		refs[i] = ref{k.ID, k.Name}
	}
	r, err := pick("ssh key", pos[0], refs)
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Delete ssh key %s?", r.Name)); err != nil {
		return err
	}
	if err := c.call(http.MethodDelete, "/api/me/ssh-keys/"+r.ID, nil, nil); err != nil {
		return err
	}
	return c.done(nil, "deleted ssh key %s", r.Name)
}
