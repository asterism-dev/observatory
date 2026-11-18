#!/usr/bin/python3
"""Guest client of the node's token service; never prints a token except as token's stdout and credential's answer."""
import json
import os
import re
import socket
import sys
import time

PORT = 1051
TIMEOUT = 3
ROOT = os.environ.get("VMM_ROOT", "")
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
    except (OSError, ValueError):
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


def sync():
    sync_git()
    return 0


def main(argv):
    if len(argv) == 2 and argv[0] == "token":
        return token(argv[1])
    if len(argv) == 2 and argv[0] == "credential":
        return credential(argv[1])
    if argv == ["sync"]:
        return sync()
    sys.stderr.write("usage: vmm-integrations.py token <kind> | credential <action> | sync\n")
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
