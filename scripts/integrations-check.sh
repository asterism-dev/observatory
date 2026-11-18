#!/bin/sh
# usage: [SSH=<ssh command>] integrations-check.sh <ssh-destination> [<owner>/<private-repo>] — in-guest checks, never prints a token
usage() {
	echo "usage: $0 <ssh-destination> [<owner>/<private-repo>]" >&2
	exit 2
}
[ $# -ge 1 ] && [ $# -le 2 ] || usage
# The repo reaches a remote shell, so only name characters pass.
case ${2:-} in *[!A-Za-z0-9._/-]*) usage ;; esac
dest=$1
shift
# shellcheck disable=SC2086
exec ${SSH:-ssh} "$dest" sh -s -- "$@" <<'GUEST'
repo=${1:-}
fails=0
pass() { echo "PASS $1"; }
fail() { echo "FAIL $1"; fails=$((fails + 1)); }
check() {
	name=$1
	shift
	if "$@" >/dev/null 2>&1; then pass "$name"; else fail "$name"; fi
}

modes=
for kind in github claude linear; do
	if tok=$(vmm-token "$kind" 2>/dev/null); then
		case $tok in obsph_*) mode=proxy ;; *) mode=vm ;; esac
		modes="$modes $mode"
		pass "vmm-token $kind ($mode)"
	else
		fail "vmm-token $kind (exit $?)"
	fi
done
unset tok

check "gh api user" sh -c 'login=$(gh api user --jq .login) && test -n "$login"'
if [ -n "$repo" ]; then
	check "git ls-remote https://github.com/$repo" env GIT_TERMINAL_PROMPT=0 git ls-remote "https://github.com/$repo"
	check "git ls-remote git@github.com:$repo.git" env GIT_TERMINAL_PROMPT=0 git ls-remote "git@github.com:$repo.git"
else
	echo "SKIP git ls-remote (no repo given)"
fi
# -w: an auth error ("…access token is invalid") contains "ok" too.
check "claude -p" sh -c 'timeout 180 claude -p "say ok" </dev/null | grep -qiw ok'
check "claude mcp list: linear connected" sh -c 'timeout 60 claude mcp list </dev/null | grep -q "^linear:.*Connected"'
check "linear graphql viewer email" sh -c 'key=${LINEAR_API_KEY:-$(vmm-token linear)} &&
	curl -fsS -H "Authorization: $key" -H "Content-Type: application/json" -d "{\"query\":\"{ viewer { email } }\"}" https://api.linear.app/graphql |
	grep -q "\"email\":\"[^\"]"'

case $modes in
*vm*) echo "SKIP leak check (a kind is delivered inside the VM)" ;;
*)
	# Brackets keep the literal prefixes out of this command line (sudo copies it into SUDO_COMMAND).
	pattern='gh[p]_|github_[p]at_|sk-an[t]-|lin_ap[i]_'
	sudo=
	if sudo -n true 2>/dev/null; then sudo="sudo -n"; fi
	leaks=$($sudo sh -c 'for f in /proc/[0-9]*/environ; do grep -qaE "$1" "$f" 2>/dev/null && echo "$f"; done
		grep -rlaE "$1" "$2/.claude" "$2/.claude.json" "$2/.git-credentials" /etc/environment /etc/vmm 2>/dev/null' sh "$pattern" "$HOME")
	if [ -n "$leaks" ]; then
		for f in $leaks; do fail "leak check: token-like value in $f"; done
	else
		pass "leak check${sudo:+ (all processes)}"
	fi
	;;
esac

exit $((fails > 0))
GUEST
