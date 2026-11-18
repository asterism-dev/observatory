#!/bin/sh
# shellcheck disable=SC2046,SC2086
set -eu

seed=$1
version=$2
tmp=/run/vmm-build
step=start
failed=

log() {
	echo "vmm-image-build: $*"
	echo "vmm-image-build: $*" >/dev/hvc0 2>/dev/null || true
}

fail() {
	failed=1
	log "FAIL $step: $*"
	exit 1
}

on_exit() {
	rc=$?
	[ "$rc" -eq 0 ] || [ -n "$failed" ] || log "FAIL $step: exit status $rc"
}
trap on_exit EXIT

begin() {
	step=$1
	log "step $step"
}

# mf <path...>: list elements are picked by name; a list prints its names, a map its key=value pairs.
mf() {
	python3 -c '
import json, sys
v = json.load(open(sys.argv[1]))
for k in sys.argv[2:]:
    v = next(x for x in v if x["name"] == k) if isinstance(v, list) else v[k]
if isinstance(v, dict):
    v = " ".join(k + "=" + x for k, x in sorted(v.items()))
elif isinstance(v, list):
    v = " ".join(x["name"] if isinstance(x, dict) else x for x in v)
print(v)
' "$seed/manifest.json" "$@"
}

apt_get() {
	DEBIAN_FRONTEND=noninteractive apt-get -y -q --snapshot "$snapshot" \
		-o Dir::Cache::Archives="$tmp/apt/" "$@"
}

fetch() {
	url=$(mf files "$1" url)
	sum=$(mf files "$1" sha256)
	curl -fsSL --proto =https --proto-redir =https --retry 3 -o "$tmp/$1" "$url" || fail "download $url"
	echo "$sum  $tmp/$1" | sha256sum -c --quiet - || fail "$1: checksum mismatch"
}

install_file() {
	f=$tmp/$1
	case $1 in
	node)
		tar -xJf "$f" -C /usr/local --strip-components=1 --no-same-owner
		;;
	claude)
		install -m 0755 "$f" /usr/local/bin/claude
		;;
	codex)
		tar -xzf "$f" -C "$tmp"
		install -m 0755 "$tmp/codex-x86_64-unknown-linux-musl" /usr/local/bin/codex
		rm -f "$tmp/codex-x86_64-unknown-linux-musl"
		;;
	uv)
		tar -xzf "$f" -C "$tmp"
		install -m 0755 "$tmp/uv-x86_64-unknown-linux-gnu/uv" "$tmp/uv-x86_64-unknown-linux-gnu/uvx" /usr/local/bin/
		rm -rf "$tmp/uv-x86_64-unknown-linux-gnu"
		;;
	chrome)
		unzip -q "$f" -d "$tmp"
		rm -rf /opt/vmm/chrome
		mkdir -p /opt/vmm
		mv "$tmp/chrome-linux64" /opt/vmm/chrome
		chmod -R go-w /opt/vmm/chrome
		;;
	gh)
		tar -xzf "$f" -C "$tmp"
		install -m 0755 "$tmp/gh_$(mf files gh version)_linux_amd64/bin/gh" /usr/bin/gh
		rm -rf "$tmp/gh_$(mf files gh version)_linux_amd64"
		;;
	check-config)
		return 0
		;;
	*)
		fail "no install rule for $1"
		;;
	esac
	rm -f "$f"
}

check_docker() {
	NO_COLOR=1 sh "$tmp/check-config" >"$tmp/check-config.out" 2>&1
}

expect() {
	case $1 in
	docker) v=$(mf repos docker packages docker-ce) ;;
	xpra) v=$(mf repos xpra packages xpra) ;;
	agent-browser) v=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["dependencies"]["agent-browser"])' "$seed/agent-browser-package.json") ;;
	*) v=$(mf files "$1" version) ;;
	esac
	v=${v#*:}
	echo "${v%%-*}"
}

verify() {
	case $1 in
	node) out=$(node --version 2>&1) ;;
	claude) out=$(claude --version 2>&1) ;;
	codex) out=$(codex --version 2>&1) ;;
	uv) out=$(uv --version 2>&1) ;;
	chrome) out=$(/opt/vmm/chrome/chrome --no-sandbox --version 2>&1) ;;
	docker) out=$(docker --version 2>&1) ;;
	gh) out=$(/usr/bin/gh --version 2>&1) ;;
	xpra) out=$(xpra --version 2>&1) ;;
	agent-browser) out=$(setpriv --reuid=nobody --regid=nogroup --clear-groups env HOME=/nonexistent /opt/vmm/agent-browser/bin/agent-browser --version 2>&1) ;;
	check-config) return 0 ;;
	*) fail "no version check for $1" ;;
	esac
	want=$(expect "$1")
	case " $out " in
	*[!0-9.]"$want"[!0-9.]*) log "$1 $want" ;;
	*) fail "$1 version: want $want, got $(echo "$out" | head -n 1)" ;;
	esac
}

[ "$(id -u)" -eq 0 ] || fail "must run as root"
[ -n "$version" ] || fail "no version given"
snapshot=$(mf aptSnapshot)

begin tmpfs
mkdir -p "$tmp"
mount -t tmpfs -o size=1G,mode=0700 tmpfs "$tmp"
mkdir -p "$tmp/apt/partial" "$tmp/home"
# Caches and dotfiles of gpg, npm and the tools' --version runs land in tmpfs, not in the image.
export HOME="$tmp/home"

begin apt
apt_get update
apt_pkgs=$(mf apt)
apt_get install --no-install-recommends $apt_pkgs
apt_get clean

begin repos
install -d -m 0755 /etc/apt/keyrings
pkgs=
holds=
hosts=
repos=$(mf repos)
for name in $repos; do
	fpr=$(mf repos "$name" fingerprint)
	curl -fsSL --proto =https --proto-redir =https --retry 3 -o "$tmp/$name.key" "$(mf repos "$name" keyURL)" || fail "$name key download"
	primary=$(gpg --show-keys --with-colons "$tmp/$name.key" | awk -F: '/^pub:/ { p = 1; next } p && /^fpr:/ { print $10; p = 0 }')
	echo "$primary" | grep -qx "$fpr" || fail "$name key fingerprint is not $fpr (got $(echo "$primary" | tr '\n' ' '))"
	gpg --batch --quiet --import "$tmp/$name.key"
	gpg --batch --export "$fpr" >"/etc/apt/keyrings/$name.gpg"
	chmod 0644 "/etc/apt/keyrings/$name.gpg"
	url=$(mf repos "$name" url)
	host=${url#https://}
	host=${host%%/*}
	hosts="$hosts $host"
	echo "deb [signed-by=/etc/apt/keyrings/$name.gpg] $url $(mf repos "$name" suite) $(mf repos "$name" component)" >"/etc/apt/sources.list.d/$name.list"
	repo_pkgs=$(mf repos "$name" packages)
	pkgs="$pkgs $repo_pkgs"
	# Only the listed versions may come from this repo; nothing else from it can replace a snapshot package.
	printf 'Package: *\nPin: origin %s\nPin-Priority: -1\n' "$host" >"/etc/apt/preferences.d/vmm-$name"
	for p in $repo_pkgs; do
		holds="$holds ${p%%=*}"
		printf '\nPackage: %s\nPin: version %s\nPin-Priority: 1001\n' "${p%%=*}" "${p#*=}" >>"/etc/apt/preferences.d/vmm-$name"
	done
done
gpgconf --kill all
apt_get update
apt_get install --no-install-recommends $pkgs
apt-mark hold $holds
apt_get clean
for host in $hosts; do
	awk '/^Package: /{ p = $2 } /^Version: /{ print p "=" $2 }' /var/lib/apt/lists/"${host}"_*_Packages
done | sort -u >"$tmp/offered"
[ -s "$tmp/offered" ] || fail "no package lists for$hosts"
dpkg-query -W -f='${db:Status-Status} ${Package}=${Version}\n' | awk '$1 == "installed" { print $2 }' | sort -u >"$tmp/installed"
printf '%s\n' $pkgs >"$tmp/pinned"
unpinned=$(comm -12 "$tmp/offered" "$tmp/installed" | grep -vxF -f "$tmp/pinned" || true)
[ -z "$unpinned" ] || fail "installed from a third-party repo but not in the manifest: $(echo "$unpinned" | tr '\n' ' ')"

begin files
files=$(mf files)
for name in $files; do
	fetch "$name"
	install_file "$name"
done

begin agent-browser
ab=/opt/vmm/agent-browser
mkdir -p "$ab/bin"
cp "$seed/agent-browser-package.json" "$ab/package.json"
cp "$seed/agent-browser-package-lock.json" "$ab/package-lock.json"
(cd "$ab" && npm ci --omit=dev --ignore-scripts --no-audit --no-fund)
ln -sfn ../node_modules/@agent-browser/cli-linux-x64-gnu/bin/agent-browser "$ab/bin/agent-browser"

begin docker
install -D -m 0644 "$seed/docker-daemon.json" /etc/docker/daemon.json
systemctl enable docker.socket
systemctl disable docker.service containerd.service
if ! check_docker; then
	log "check-config reports a missing required option, installing linux-modules-extra"
	apt_get install --no-install-recommends "linux-modules-extra-$(uname -r)" || fail "cannot install linux-modules-extra-$(uname -r)"
	apt_get clean
	check_docker || fail "check-config: $(sed -n '/^Generally Necessary:/,/^Optional Features:/p' "$tmp/check-config.out" | grep -E 'missing|nonexistent|disabled' | tr -s ' \n' ' ')"
fi
rm -f "$tmp/check-config" "$tmp/check-config.out"

begin config
# The xpra package enables a system proxy on TCP 14500 and /run/xpra; the desktop is a per-user unit reached over SSH only.
systemctl mask xpra-server.socket xpra-server.service
echo "prefix=\${HOME}/.local" >/usr/local/etc/npmrc
grep -qx 'DISABLE_AUTOUPDATER=1' /etc/environment || echo 'DISABLE_AUTOUPDATER=1' >>/etc/environment
# Noble restricts unprivileged user namespaces to profiled binaries; Chrome's sandbox needs them.
cat >/etc/apparmor.d/vmm-chrome <<'EOF'
abi <abi/4.0>,
include <tunables/global>

profile vmm-chrome /opt/vmm/chrome/chrome flags=(unconfined) {
  userns,
  include if exists <local/vmm-chrome>
}
EOF

begin verify
for name in $files $repos agent-browser; do
	verify "$name"
done

begin clean
apt-get clean
rm -rf /var/lib/apt/lists/*
cloud-init clean --logs --machine-id
rm -f /etc/ssh/ssh_host_*
fstrim -av || true
umount "$tmp"
sync
install -d -m 0755 /etc/observatory
printf '%s\n' "$version" >/etc/observatory/image
chmod 0644 /etc/observatory/image
sync
log "done $version"
