#!/usr/bin/python3
"""Guest client of the node's token service; never prints a token except as token's stdout and credential's answer."""
import hashlib
import json
import os
import pwd
import re
import shutil
import socket
import subprocess
import sys
import time

PORT = 1051
TIMEOUT = 3
ROOT = os.environ.get("VMM_ROOT", "")
USER = os.environ.get("VMM_USER", "ubuntu")
STATE_DIR = ROOT + "/var/lib/vmm/" + USER
LINEAR_MCP = "https://mcp.linear.app/mcp"
# A token with a newline would inject lines into git's credential protocol.
TOKEN = re.compile(r"[A-Za-z0-9._~+/=_-]{12,4096}")
NOT_CONNECTED, UNAVAILABLE = 3, 4


def fetch(kind):
    try:
        path = os.environ.get("VMM_TOKEN_UNIX")
        if path:
            s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            addr = path
        else:
            s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
            addr = (socket.VMADDR_CID_HOST, PORT)
        with s:
            deadline = time.monotonic() + TIMEOUT
            s.settimeout(TIMEOUT)
            s.connect(addr)
            s.sendall(json.dumps({"op": "token", "kind": kind}).encode() + b"\n")
            buf = b""
            while b"\n" not in buf and len(buf) < 16 << 10:
                left = deadline - time.monotonic()
                if left <= 0:
                    return UNAVAILABLE, None
                s.settimeout(left)
                chunk = s.recv(4096)
                if not chunk:
                    break
                buf += chunk
        reply = json.loads(buf.split(b"\n", 1)[0])
    except (OSError, ValueError, AttributeError):
        # AttributeError: a Python without socket.AF_VSOCK.
        return UNAVAILABLE, None
    if not isinstance(reply, dict):
        return UNAVAILABLE, None
    if reply.get("ok") is True:
        tok = reply.get("token")
        if isinstance(tok, str) and TOKEN.fullmatch(tok.strip()):
            return 0, tok.strip()
        return UNAVAILABLE, None
    if reply.get("error") == "not-connected":
        return NOT_CONNECTED, None
    return UNAVAILABLE, None


def token(kind):
    code, tok = fetch(kind)
    if code == 0:
        sys.stdout.write(tok + "\n")
    return code


def credential(action):
    attrs = {}
    for line in sys.stdin:
        line = line.rstrip("\n")
        if not line:
            break
        k, _, v = line.partition("=")
        attrs[k] = v
    if action != "get" or attrs.get("protocol") != "https" or attrs.get("host") != "github.com":
        return 0
    code, tok = fetch("github")
    if code == 0:
        sys.stdout.write("username=x-access-token\npassword=%s\n" % tok)
    return 0


def sync_git():
    code, _ = fetch("github")
    if code == UNAVAILABLE:
        return
    path = ROOT + "/etc/gitconfig"
    include = ["[include]\n", "\tpath = %s/etc/vmm/gitconfig\n" % ROOT]
    try:
        with open(path, encoding="utf-8", errors="surrogateescape", newline="") as f:
            old = f.read()
    except FileNotFoundError:
        old = ""
    lines, keep = old.splitlines(True), []
    i = 0
    while i < len(lines):
        if lines[i:i + 2] == include:
            i += 2
            continue
        keep.append(lines[i])
        i += 1
    if keep and not keep[-1].endswith("\n"):
        keep[-1] += "\n"
    if code == 0:
        keep += include
    new = "".join(keep)
    if new == old:
        return
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = path + ".vmm-new"
    with open(tmp, "w", encoding="utf-8", errors="surrogateescape", newline="") as f:
        f.write(new)
    os.chmod(tmp, 0o644)
    os.replace(tmp, path)


def log(msg):
    sys.stderr.write("vmm-integrations: %s\n" % msg)


def digest(tok):
    return hashlib.sha256(tok.encode()).hexdigest()


def write_private(path, text):
    tmp = path + ".vmm-new"
    if os.path.lexists(tmp):
        os.remove(tmp)
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as f:
        f.write(text)
    os.replace(tmp, path)


def file_token_hash(path):
    """Hash of the file's claudeAiOauth.accessToken; "" when missing, None when unreadable or without one."""
    try:
        with open(path, encoding="utf-8") as f:
            tok = json.load(f)["claudeAiOauth"]["accessToken"]
    except FileNotFoundError:
        return ""
    except (OSError, ValueError, KeyError, TypeError):
        return None
    return digest(tok) if isinstance(tok, str) else None


def sync_claude(home, state):
    code, tok = fetch("claude")
    if code == UNAVAILABLE:
        return
    path = home + "/.claude/.credentials.json"
    current = file_token_hash(path)
    ours = current is not None and current == state.get("claude")
    if code == NOT_CONNECTED:
        if ours:
            os.remove(path)
        state.pop("claude", None)
        return
    if current != "" and not ours:
        # The user's own login (or a file we cannot read): never ours to replace.
        state.pop("claude", None)
        return
    if current != digest(tok):
        os.makedirs(home + "/.claude", 0o700, exist_ok=True)
        os.chmod(home + "/.claude", 0o700)
        cred = {"claudeAiOauth": {"accessToken": tok, "refreshToken": None, "expiresAt": None, "scopes": ["user:inference"]}}
        write_private(path, json.dumps(cred, separators=(",", ":")))
    state["claude"] = digest(tok)


def claude_cli(claude, home, *args):
    # The add call carries the token on argv; never let the CLI echo it into the journal.
    try:
        return subprocess.run([claude, *args], env=dict(os.environ, HOME=home), stdin=subprocess.DEVNULL,
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=60).returncode
    except (OSError, subprocess.SubprocessError):
        return -1


def sync_linear(home, state):
    claude = shutil.which("claude", path=home + "/.local/bin" + os.pathsep + os.environ.get("PATH", os.defpath))
    if not claude:
        return
    code, tok = fetch("linear")
    if code == UNAVAILABLE:
        return
    recorded = state.get("linear_mcp")
    if code == 0 and recorded == digest(tok):
        return
    if recorded:
        # Keep the record on failure, so the next sync retries instead of treating the entry as the user's.
        if claude_cli(claude, home, "mcp", "remove", "--scope", "user", "linear") != 0:
            log("claude mcp remove failed")
            return
        state.pop("linear_mcp")
    if code == NOT_CONNECTED:
        return
    if not recorded and claude_cli(claude, home, "mcp", "get", "linear") == 0:
        return
    if claude_cli(claude, home, "mcp", "add", "--transport", "http", "--scope", "user", "linear", LINEAR_MCP,
                  "--header", "Authorization: Bearer " + tok) == 0:
        state["linear_mcp"] = digest(tok)
    else:
        log("claude mcp add failed")


def sync_user(home):
    path = STATE_DIR + "/integrations.json"
    try:
        with open(path, encoding="utf-8") as f:
            state = json.load(f)
    except (OSError, ValueError):
        state = {}
    if not isinstance(state, dict):
        state = {}
    old = dict(state)
    for name, part in (("claude credentials", sync_claude), ("linear mcp", sync_linear)):
        try:
            part(home, state)
        except OSError:
            log("%s: write failed" % name)
    if state != old:
        try:
            write_private(path, json.dumps(state))
        except OSError:
            log("state: write failed")
    return 0


def sync():
    try:
        sync_git()
    except OSError:
        log("gitconfig: write failed")
    try:
        pw = pwd.getpwnam(USER)
    except KeyError:
        return 0
    try:
        os.makedirs(STATE_DIR, 0o700, exist_ok=True)
        # Through an fd opened without following links: root must never chown what a link points to.
        fd = os.open(STATE_DIR, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            os.fchown(fd, pw.pw_uid, pw.pw_gid)
            os.fchmod(fd, 0o700)
        finally:
            os.close(fd)
    except OSError:
        log("state dir: write failed")
        return 0
    if ROOT:
        return sync_user(ROOT + "/home/" + USER)
    # Files in the user's home are touched only as the user, so a planted link never gets root access.
    try:
        subprocess.run(["runuser", "-u", USER, "--", "env", "HOME=" + pw.pw_dir, "python3", "-I", os.path.abspath(__file__), "sync-user"],
                       stdin=subprocess.DEVNULL, timeout=300)
    except (OSError, subprocess.SubprocessError):
        log("user sync failed")
    return 0


def main(argv):
    if len(argv) == 2 and argv[0] == "token":
        return token(argv[1])
    if len(argv) == 2 and argv[0] == "credential":
        return credential(argv[1])
    if argv == ["sync"]:
        return sync()
    if argv == ["sync-user"]:
        return sync_user(os.environ.get("HOME", ""))
    sys.stderr.write("usage: vmm-integrations.py token <kind> | credential <action> | sync | sync-user\n")
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
