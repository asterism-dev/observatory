#!/bin/sh
# Replaces the hub-managed SSH keys and variables with the seed's ($1); runs as root on every boot.
# Never echo values here: the output ends up in the VM's console log on the node.
set -u
seed=$1
root=${VMM_ROOT:-}
umask 022
# Constant, so a VM's first egress CA needs no restart of anything already reading them.
bundle=/etc/ssl/certs/ca-certificates.crt
trust_vars='SSL_CERT_FILE NODE_EXTRA_CA_CERTS REQUESTS_CA_BUNDLE CURL_CA_BUNDLE'
mkdir -p "$root/etc/vmm" "$root/etc/ssh/sshd_config.d" "$root/etc/profile.d"

cp "$seed/vmm-authorized-keys" "$root/etc/vmm/authorized_keys.new" &&
	chmod 644 "$root/etc/vmm/authorized_keys.new" &&
	mv "$root/etc/vmm/authorized_keys.new" "$root/etc/vmm/authorized_keys"
# Only for ubuntu: hub keys must never log in as root.
printf 'Match User ubuntu\n\tAuthorizedKeysFile .ssh/authorized_keys /etc/vmm/authorized_keys\n' >"$root/etc/ssh/sshd_config.d/50-vmm.conf"
echo 'if [ -r /etc/vmm/env.sh ]; then . /etc/vmm/env.sh; fi' >"$root/etc/profile.d/vmm-env.sh"

: >"$root/etc/vmm/env.sh"
python3 - "$seed/vmm-env" "$root" "$bundle" "$trust_vars" <<'PY'
import json, os, re, sys

src, root = sys.argv[1], sys.argv[2]
with open(src, encoding="utf-8") as f:
    env = json.load(f) or {}
# A name that is not a shell identifier would inject code into env.sh.
names = [n for n, v in env.items() if re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", n) and "\0" not in v]
env = {**{n: sys.argv[3] for n in sys.argv[4].split()}, **{n: env[n] for n in names}}
names = sorted(env)


def write(path, text):
    tmp = path + ".new"
    with open(tmp, "w", encoding="utf-8", errors="surrogateescape", newline="") as f:
        f.write(text)
    os.chmod(tmp, 0o644)
    os.replace(tmp, path)


def quote(v):
    return "'" + v.replace("'", "'\\''") + "'"


write(root + "/etc/vmm/env.sh", "".join("export %s=%s\n" % (n, quote(env[n])) for n in names))

path = root + "/etc/environment"
try:
    with open(path, encoding="utf-8", errors="surrogateescape", newline="") as f:
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
# pam_env strips the outer quotes but cuts values at "#" and splits long lines into new KEY=VAL lines; such values live in env.sh only.
block = ['%s="%s"\n' % (n, env[n]) for n in names if not re.search(r"[\r\n#]", env[n]) and len(env[n].encode("utf-8", "surrogateescape")) <= 1000]
if block:
    keep += ["# BEGIN vmm\n"] + block + ["# END vmm\n"]
write(path, "".join(keep))
PY

python3 "$seed/vmm-agent.py" install-cas "$seed/vmm-egress-cas"
reexec=
mkdir -p "$root/etc/systemd/system.conf.d" "$root/etc/systemd/user.conf.d"
for f in "$root/etc/systemd/system.conf.d/50-vmm-trust.conf" "$root/etc/systemd/user.conf.d/50-vmm-trust.conf"; do
	printf '[Manager]\nDefaultEnvironment=' >"$f.new"
	for n in $trust_vars; do printf '%s=%s ' "$n" "$bundle"; done >>"$f.new"
	echo >>"$f.new"
	if cmp -s "$f.new" "$f"; then rm -f "$f.new"; else chmod 644 "$f.new" && mv "$f.new" "$f" && reexec=1; fi
done

changed=
mkdir -p "$root/usr/local/lib/vmm" "$root/etc/systemd/system"
cp "$seed/vmm-agent.py" "$root/usr/local/lib/vmm/vmm-agent.py.new"
printf '[Unit]\nDescription=vmm guest agent\n\n[Service]\nExecStart=/usr/bin/python3 /usr/local/lib/vmm/vmm-agent.py\nRestart=always\nRestartSec=2\n\n[Install]\nWantedBy=multi-user.target\n' >"$root/etc/systemd/system/vmm-agent.service.new"
for f in "$root/usr/local/lib/vmm/vmm-agent.py" "$root/etc/systemd/system/vmm-agent.service"; do
	if cmp -s "$f.new" "$f"; then rm -f "$f.new"; else chmod 644 "$f.new" && mv "$f.new" "$f" && changed=1; fi
done

reload=
# Static glue only: the scripts ask the node's token service on demand, so no token is ever written here.
lib=$root/usr/local/lib/vmm bin=$root/usr/local/bin units=$root/etc/systemd/system
mkdir -p "$bin" "$root/usr/local/share/vmm" "$root/etc/claude-code"
cp "$seed/vmm-integrations.py" "$lib/vmm-integrations.py.new"
cp "$seed/vmm-AGENTS.md" "$root/usr/local/share/vmm/AGENTS.md.new"
printf '#!/bin/sh\nexec python3 /usr/local/lib/vmm/vmm-integrations.py token "$@"\n' >"$bin/vmm-token.new"
printf '#!/bin/sh\nexec python3 /usr/local/lib/vmm/vmm-integrations.py credential "$@"\n' >"$bin/git-credential-vmm.new"
printf '[credential "https://github.com"]\n\thelper = vmm\n[url "https://github.com/"]\n\tinsteadOf = git@github.com:\n\tinsteadOf = ssh://git@github.com/\n' >"$root/etc/vmm/gitconfig.new"
cat >"$bin/gh.new" <<'EOF'
#!/bin/sh
# A user's own token or gh login wins over the VM's GitHub integration.
if [ -z "${GH_TOKEN:-}" ] && [ -z "${GITHUB_TOKEN:-}" ] && ! grep -qs '^github\.com:' "${GH_CONFIG_DIR:-$HOME/.config/gh}/hosts.yml" && vmm_token=$(vmm-token github); then
	GH_TOKEN=$vmm_token
	export GH_TOKEN
fi
exec "${VMM_GH_BIN:-/usr/bin/gh}" "$@"
EOF
cat >"$root/etc/profile.d/vmm-linear.sh.new" <<'EOF'
if [ -z "${LINEAR_API_KEY+x}" ] && command -v vmm-token >/dev/null 2>&1 && vmm_linear=$(vmm-token linear); then export LINEAR_API_KEY="$vmm_linear"; fi
unset vmm_linear
EOF
printf '[Unit]\nDescription=vmm integrations sync\n\n[Service]\nType=oneshot\nTimeoutStartSec=6min\nExecStart=/usr/bin/python3 /usr/local/lib/vmm/vmm-integrations.py sync\n' >"$units/vmm-integrations.service.new"
printf '[Unit]\nDescription=vmm integrations sync\n\n[Timer]\nOnBootSec=30s\nOnUnitActiveSec=5min\n\n[Install]\nWantedBy=timers.target\n' >"$units/vmm-integrations.timer.new"
for f in "$lib/vmm-integrations.py" "$root/usr/local/share/vmm/AGENTS.md" "$root/etc/vmm/gitconfig" "$root/etc/profile.d/vmm-linear.sh" "$units/vmm-integrations.service" "$units/vmm-integrations.timer" "$bin/vmm-token" "$bin/git-credential-vmm" "$bin/gh"; do
	case $f in "$bin"/*) mode=755 ;; *) mode=644 ;; esac
	if cmp -s "$f.new" "$f"; then rm -f "$f.new"; else chmod $mode "$f.new" && mv "$f.new" "$f" && case $f in "$units"/*) reload=1 ;; esac; fi
done
desktop=
user_units=$root/etc/systemd/user
mkdir -p "$user_units" "$root/var/lib/systemd/linger"
cp "$seed/vmm-desktop" "$bin/vmm-desktop.new"
cp "$seed/vmm-browser.py" "$lib/vmm-browser.py.new"
printf '#!/bin/sh\nexec python3 /usr/local/lib/vmm/vmm-browser.py "$@"\n' >"$bin/agent-browser.new"
# xpra 6.5: an empty --bind-tcp= is rejected (no TCP socket is the default), bind=auto would add an abstract socket, and systemd-run would move xpra out of this unit's cgroup.
# A custom --xvfb gets no -auth from xpra; without it any local UID could drive the X server ($$ leaves xpra a literal $XAUTHORITY).
cat >"$user_units/vmm-desktop.service.new" <<'EOF'
[Unit]
Description=vmm desktop (xpra on :100)

[Service]
ExecStart=/usr/bin/xpra start :100 --daemon=no --systemd-run=no --bind=noabstract --socket-dir=%t/xpra --mdns=no --html=off --pulseaudio=no --printing=no --file-transfer=off --notifications=no --clipboard=yes "--xvfb=Xvfb +extension GLX +extension Composite -screen 0 5120x2880x24+32 -nolisten tcp -nolisten local -noreset -auth $$XAUTHORITY -dpi 96"
EOF
printf '[Unit]\nDescription=vmm desktop idle stop\n\n[Service]\nType=oneshot\nExecStart=/usr/local/bin/vmm-desktop idle-check\n' >"$user_units/vmm-desktop-idle.service.new"
printf '[Unit]\nDescription=vmm desktop idle stop\nPartOf=vmm-desktop.service\n\n[Timer]\nOnActiveSec=1min\nOnUnitActiveSec=1min\n' >"$user_units/vmm-desktop-idle.timer.new"
for f in "$bin/vmm-desktop" "$lib/vmm-browser.py" "$bin/agent-browser" "$user_units/vmm-desktop.service" "$user_units/vmm-desktop-idle.service" "$user_units/vmm-desktop-idle.timer"; do
	case $f in "$bin"/*) mode=755 ;; *) mode=644 ;; esac
	if cmp -s "$f.new" "$f"; then rm -f "$f.new"; else chmod $mode "$f.new" && mv "$f.new" "$f" && case $f in "$user_units"/*) desktop=1 ;; esac; fi
done
# Linger runs ubuntu's user manager without a login, so the wrappers can start the desktop; the file works before cloud-init creates the user.
[ -e "$root/var/lib/systemd/linger/ubuntu" ] || touch "$root/var/lib/systemd/linger/ubuntu"

# Claude Code's managed memory; a file or link the admin put there stays.
[ -e "$root/etc/claude-code/CLAUDE.md" ] || [ -L "$root/etc/claude-code/CLAUDE.md" ] || ln -s /usr/local/share/vmm/AGENTS.md "$root/etc/claude-code/CLAUDE.md"

# A running sshd reads its config only on start or reload.
if [ -z "$root" ] && command -v systemctl >/dev/null 2>&1; then
	systemctl try-reload-or-restart ssh.service >/dev/null 2>&1
	# Only a re-executed manager reads DefaultEnvironment again; it reloads the units as well.
	if [ -n "$reexec" ]; then
		systemctl daemon-reexec >/dev/null 2>&1
	elif [ -n "$changed$reload" ]; then
		systemctl daemon-reload >/dev/null 2>&1
	fi
	# enable reloads systemd, which costs a fast-booted VM half a second.
	systemctl is-enabled -q vmm-agent.service || systemctl enable vmm-agent.service >/dev/null 2>&1
	# --no-block: at boot this runs inside cloud-init, which the unit is ordered after; under the agent (VMM_AGENT) a restart would kill its own request.
	if [ -n "$changed" ] && [ -z "${VMM_AGENT:-}" ]; then
		systemctl --no-block restart vmm-agent.service >/dev/null 2>&1
	else
		systemctl --no-block start vmm-agent.service >/dev/null 2>&1
	fi
	systemctl is-enabled -q vmm-integrations.timer || systemctl enable vmm-integrations.timer >/dev/null 2>&1
	systemctl --no-block start vmm-integrations.timer vmm-integrations.service >/dev/null 2>&1
	if id ubuntu >/dev/null 2>&1; then
		loginctl enable-linger ubuntu >/dev/null 2>&1
		[ -z "$desktop" ] || systemctl --user -M ubuntu@ daemon-reload >/dev/null 2>&1
	fi
fi
exit 0
