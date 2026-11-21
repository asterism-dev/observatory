package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
)

func connectCommands() []command {
	return []command{
		{"ssh", "<vm> [--user dev] [-- args...]", "ssh into a VM", cmdSSH},
		{"desktop", "<vm> [--user dev]", "start the VM desktop and attach with xpra", cmdDesktop},
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
	switch v.State {
	case "stopped", "paused", "hibernated":
		return v, "", &exitError{1, fmt.Sprintf("vm %s is %s; run observatory vm start %s", v.Name, v.State, v.Name)}
	}
	host := v.DNSName
	if host == "" {
		host = v.MgmtIP
	}
	if host == "" {
		return v, "", &exitError{1, "vm has no management address yet"}
	}
	return v, host, nil
}

func (c *cli) warnNoDevice() {
	me, err := c.me()
	if err != nil {
		return
	}
	path, err := c.orgPath("/devices")
	if err != nil {
		return
	}
	var devices []deviceJSON
	if err := c.call(http.MethodGet, path, nil, &devices); err != nil {
		return
	}
	for _, d := range devices {
		if d.Owner.ID == me.GetUser().GetId() {
			return
		}
	}
	fmt.Fprintf(c.stderr, "observatory: no device in org %s; run observatory device create <name> and import it\n", c.org.GetName())
}

var validSSHUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}[$]?$`)

func (c *cli) sshTarget(cmdPath string, args []string, max int) (target string, rest []string, err error) {
	fs := c.flags(cmdPath)
	user := fs.String("user", "dev", "login `user`")
	pos, err := c.parse(fs, args, 1, max)
	if err != nil {
		return "", nil, err
	}
	if !validSSHUser.MatchString(*user) {
		return "", nil, usagef("invalid --user %q", *user)
	}
	_, host, err := c.sshHost(pos[0])
	if err != nil {
		return "", nil, err
	}
	if strings.HasPrefix(host, "-") {
		return "", nil, &exitError{1, "invalid vm address " + host}
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

func cmdSSHKeyList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("ssh-key list"), args, 0, 0); err != nil {
		return err
	}
	res := &iamv1.IAMServiceListSSHKeysResponse{}
	if err := c.rpc("ListSSHKeys", &iamv1.IAMServiceListSSHKeysRequest{}, res); err != nil {
		return err
	}
	rows := make([][]string, len(res.GetKeys()))
	for i, k := range res.GetKeys() {
		rows[i] = []string{k.GetId(), k.GetName(), k.GetFingerprint(), ago(c.now(), k.GetCreatedAt().AsTime())}
	}
	return c.emit(pj(res), []string{"id", "name", "fingerprint", "added"}, rows)
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
	res := &iamv1.CreateSSHKeyResponse{}
	if err := c.rpc("CreateSSHKey", &iamv1.CreateSSHKeyRequest{Name: *name, PublicKey: key}, res); err != nil {
		return err
	}
	return c.done(pj(res), "added ssh key %s", *name)
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
	res := &iamv1.IAMServiceListSSHKeysResponse{}
	if err := c.rpc("ListSSHKeys", &iamv1.IAMServiceListSSHKeysRequest{}, res); err != nil {
		return err
	}
	refs := make([]ref, len(res.GetKeys()))
	for i, k := range res.GetKeys() {
		refs[i] = ref{k.GetId(), k.GetName()}
	}
	r, err := pick("ssh key", pos[0], refs)
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Delete ssh key %s?", r.Name)); err != nil {
		return err
	}
	if err := c.rpc("DeleteSSHKey", &iamv1.DeleteSSHKeyRequest{Key: r.ID}, &iamv1.DeleteSSHKeyResponse{}); err != nil {
		return err
	}
	return c.done(nil, "deleted ssh key %s", r.Name)
}
