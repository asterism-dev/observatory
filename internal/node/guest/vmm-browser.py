#!/usr/bin/python3
"""Runs agent-browser headed on the vmm desktop (:100) with one persistent profile; refuses DevTools endpoints off loopback."""
import os
import re
import socket
import subprocess
import sys

CHROME = "/opt/vmm/chrome/chrome"
DEFAULTS = {
    "AGENT_BROWSER_HEADED": "1",
    "AGENT_BROWSER_EXECUTABLE_PATH": CHROME,
    "AGENT_BROWSER_SESSION": "default",
    "AGENT_BROWSER_PROFILE": "~/.local/share/vmm-browser/default",
    "AGENT_BROWSER_ARGS": "--password-store=basic",
}
# Strict on purpose: userinfo or a backslash would let a URL parser pick another host than the one we checked.
LOOPBACK_CDP = re.compile(r"\d+|(?i:wss?|https?)://(127\.0\.0\.1|localhost|\[::1\])(:\d+)?([/?#].*)?", re.S)
DEBUG_ADDRESS = re.compile(r"--remote-debugging-address=([^,\s]*)")
SINGLETONS = ("SingletonLock", "SingletonSocket", "SingletonCookie")


def refuse(msg):
    print("agent-browser: " + msg, file=sys.stderr)
    sys.exit(2)


def check_devtools(argv, env):
    cdps = [env["AGENT_BROWSER_CDP"]] if env.get("AGENT_BROWSER_CDP") else []
    for i, a in enumerate(argv):
        nxt = argv[i + 1] if i + 1 < len(argv) else None
        if a == "--cdp" and nxt is not None:
            cdps.append(nxt)
        elif a.startswith("--cdp="):
            cdps.append(a[len("--cdp="):])
        elif a == "connect" and nxt is not None and re.match(r"(?i)(wss?|https?)://", nxt):
            cdps.append(nxt)
    for c in cdps:
        if not LOOPBACK_CDP.fullmatch(c):
            refuse("CDP endpoint %r is not a port or a loopback URL (127.0.0.1, localhost, [::1])" % c)
    for a in argv + [env.get("AGENT_BROWSER_ARGS", "")]:
        for addr in DEBUG_ADDRESS.findall(a):
            if addr != "127.0.0.1":
                refuse("--remote-debugging-address=%s is not 127.0.0.1" % addr)


def chrome_running(proc, uid):
    try:
        pids = [p for p in os.listdir(proc) if p.isdigit()]
    except OSError:
        return False
    for pid in pids:
        try:
            if os.stat(os.path.join(proc, pid)).st_uid == uid and os.readlink(os.path.join(proc, pid, "exe")) == CHROME:
                return True
        except OSError:
            continue
    return False


def clear_stale_lock(profile, proc, uid):
    """After a fork or a disk-only restore the lock names a host or PID that is gone, and Chrome would refuse the profile."""
    try:
        owner = os.readlink(os.path.join(profile, "SingletonLock"))
    except OSError:
        return
    host, _, pid = owner.rpartition("-")
    if host == socket.gethostname() and pid.isdigit() and os.path.exists(os.path.join(proc, pid)):
        return
    if chrome_running(proc, uid):
        return
    for name in SINGLETONS:
        try:
            os.unlink(os.path.join(profile, name))
        except FileNotFoundError:
            pass


def main():
    uid = int(os.environ.get("VMM_UID", os.getuid()))
    if uid == 0:
        print("agent-browser: run as the ubuntu user, not root", file=sys.stderr)
        sys.exit(1)
    argv = sys.argv[1:]
    env = os.environ
    check_devtools(argv, env)
    desktop = subprocess.run([env.get("VMM_DESKTOP_BIN", "/usr/local/bin/vmm-desktop"), "start"], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    if desktop.returncode != 0:
        sys.stderr.write(desktop.stdout)
        sys.exit(1)
    env["DISPLAY"] = ":100"
    for name, value in DEFAULTS.items():
        if name not in env:
            env[name] = os.path.expanduser(value)
    profile = os.path.expanduser(env["AGENT_BROWSER_PROFILE"])
    # agent-browser takes a value without "/" as the name of a profile of the system Chrome, not as a directory.
    if "/" in profile:
        clear_stale_lock(profile, env.get("VMM_PROC_ROOT", "/proc"), uid)
    binary = env.get("VMM_AGENT_BROWSER_BIN", "/opt/vmm/agent-browser/bin/agent-browser")
    os.execv(binary, [binary] + argv)


main()
