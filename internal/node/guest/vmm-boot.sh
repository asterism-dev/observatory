#!/bin/sh
# Replaces the hub-managed SSH keys and variables with the seed's ($1); runs as root on every boot.
# Never echo values here: the output ends up in the VM's console log on the node.
set -u
seed=$1
root=${VMM_ROOT:-}
umask 022
mkdir -p "$root/etc/vmm" "$root/etc/ssh/sshd_config.d" "$root/etc/profile.d"

cp "$seed/vmm-authorized-keys" "$root/etc/vmm/authorized_keys.new" &&
	chmod 644 "$root/etc/vmm/authorized_keys.new" &&
	mv "$root/etc/vmm/authorized_keys.new" "$root/etc/vmm/authorized_keys"
# Only for ubuntu: hub keys must never log in as root.
printf 'Match User ubuntu\n\tAuthorizedKeysFile .ssh/authorized_keys /etc/vmm/authorized_keys\n' >"$root/etc/ssh/sshd_config.d/50-vmm.conf"
echo 'if [ -r /etc/vmm/env.sh ]; then . /etc/vmm/env.sh; fi' >"$root/etc/profile.d/vmm-env.sh"

python3 - "$seed/vmm-env" "$root" <<'PY'
import json, os, re, sys

src, root = sys.argv[1], sys.argv[2]
with open(src, encoding="utf-8") as f:
    env = json.load(f) or {}
# A name that is not a shell identifier would inject code into env.sh.
names = sorted(n for n, v in env.items() if re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", n) and "\0" not in v)


def write(path, text):
    tmp = path + ".new"
    with open(tmp, "w", encoding="utf-8", newline="") as f:
        f.write(text)
    os.chmod(tmp, 0o644)
    os.replace(tmp, path)


def quote(v):
    return "'" + v.replace("'", "'\\''") + "'"


write(root + "/etc/vmm/env.sh", "".join("export %s=%s\n" % (n, quote(env[n])) for n in names))

path = root + "/etc/environment"
try:
    with open(path, encoding="utf-8", newline="") as f:
        lines = f.read().splitlines(True)
except FileNotFoundError:
    lines = []
keep, inside = [], False
for line in lines:
    s = line.rstrip("\n")
    if s == "# BEGIN vmm":
        inside = True
    elif s == "# END vmm":
        inside = False
    elif not inside:
        keep.append(line)
if keep and not keep[-1].endswith("\n"):
    keep[-1] += "\n"
# pam_env reads one line per variable and strips the outer quotes; multi-line values live in env.sh only.
block = ['%s="%s"\n' % (n, env[n]) for n in names if "\n" not in env[n] and "\r" not in env[n]]
if block:
    keep += ["# BEGIN vmm\n"] + block + ["# END vmm\n"]
write(path, "".join(keep))
PY

# A running sshd reads its config only on start or reload.
if [ -z "$root" ] && command -v systemctl >/dev/null 2>&1; then
	systemctl try-reload-or-restart ssh.service >/dev/null 2>&1
fi
exit 0
