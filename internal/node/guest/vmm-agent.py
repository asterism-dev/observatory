#!/usr/bin/python3
# Vsock agent for the node; requests never carry keys or variables (the seed does), so never log request values.
import base64
import glob
import json
import os
import re
import socket
import subprocess
import threading
import time
import uuid

PORT = 1050
MAX_REQUEST = 64 << 10
ROOT = os.environ.get("VMM_ROOT", "")
SEED_DIR = os.environ.get("VMM_SEED_DIR", "")
SEED_MOUNT = "/run/vmm-seed"
MAC = re.compile(r"[0-9a-f]{2}(:[0-9a-f]{2}){5}")
HOSTNAME = re.compile(r"[A-Za-z0-9]([A-Za-z0-9-]{0,62})")

lock = threading.Lock()
slots = threading.BoundedSemaphore(4)


def run(*args, env=None):
    subprocess.run(args, check=True, env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=600)


def write(path, text, mode=0o644):
    tmp = path + ".new"
    with open(tmp, "w") as f:
        f.write(text)
    os.chmod(tmp, mode)
    os.replace(tmp, path)


def mount_seed():
    if SEED_DIR:
        return SEED_DIR
    subprocess.run(["umount", SEED_MOUNT], stderr=subprocess.DEVNULL)
    os.makedirs(SEED_MOUNT, exist_ok=True)
    dev = os.path.realpath("/dev/disk/by-label/cidata")
    # The node rewrites seed.iso while the guest may still cache the old blocks.
    run("blockdev", "--flushbufs", dev)
    run("mount", "-o", "ro", dev, SEED_MOUNT)
    return SEED_MOUNT


def umount_seed():
    if not SEED_DIR:
        subprocess.run(["umount", SEED_MOUNT], stderr=subprocess.DEVNULL)


def set_clock(ns):
    if ns and not ROOT:
        time.clock_settime_ns(time.CLOCK_REALTIME, ns)


def nics():
    # Device order is the virtio index, which follows the order the node attached the NICs in.
    found = []
    for n in os.listdir("/sys/class/net"):
        dev = os.path.realpath("/sys/class/net/%s/device" % n)
        m = re.fullmatch(r"virtio(\d+)", os.path.basename(dev))
        if m:
            found.append((int(m.group(1)), n))
    return [n for _, n in sorted(found)]


def set_macs(macs):
    names = nics()
    if macs and len(macs) != len(names):
        raise ValueError("got %d macs for %d nics" % (len(macs), len(names)))
    for name, mac in zip(names, macs):
        run("ip", "link", "set", "dev", name, "down")
        run("ip", "link", "set", "dev", name, "address", mac)
        run("ip", "link", "set", "dev", name, "up")


def grow_root():
    src = subprocess.run(["findmnt", "-n", "-o", "SOURCE", "/"], check=True, capture_output=True, text=True).stdout.strip()
    part = os.path.basename(os.path.realpath(src))
    num = "/sys/class/block/%s/partition" % part
    if os.path.exists(num):
        disk = os.path.basename(os.path.dirname(os.path.realpath("/sys/class/block/" + part)))
        with open(num) as f:
            # growpart exits 1 when the partition already fills the disk.
            subprocess.run(["growpart", "/dev/" + disk, f.read().strip()], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    run("resize2fs", "/dev/" + part)


def boot(seed):
    run("sh", seed + "/vmm-boot.sh", seed, env=dict(os.environ, VMM_AGENT="1"))


def resume(req):
    set_clock(req["time"])
    seed = mount_seed()
    try:
        boot(seed)
    finally:
        umount_seed()


def takeoff(req):
    set_clock(req["time"])
    seed = mount_seed()
    try:
        if not ROOT:
            set_macs(req["macs"])
        with open(seed + "/meta-data") as f:
            m = re.search(r"^local-hostname:\s*(\S+)\s*$", f.read(), re.M)
        if m and HOSTNAME.fullmatch(m.group(1)):
            write(ROOT + "/etc/hostname", m.group(1) + "\n")
            if not ROOT:
                run("hostname", m.group(1))
        os.makedirs(ROOT + "/etc/netplan", exist_ok=True)
        with open(seed + "/network-config") as f:
            net = f.read()
        if not ROOT:
            # netplan matches the permanent MAC, which a restored NIC keeps from the snapshot; match the NICs by name instead.
            for name, mac in zip(nics(), req["macs"]):
                net = net.replace('macaddress: "%s"' % mac, 'name: "%s"' % name)
        # The seed holds a bare v2 config; netplan needs it under "network:" like cloud-init writes it.
        write(ROOT + "/etc/netplan/50-cloud-init.yaml", "network:\n" + "".join("  " + l for l in net.splitlines(True)), 0o600)
        if not ROOT:
            run("netplan", "apply")
        for k in glob.glob(ROOT + "/etc/ssh/ssh_host_*"):
            os.remove(k)
        os.makedirs(ROOT + "/etc/ssh", exist_ok=True)
        run("ssh-keygen", "-A", *(["-f", ROOT] if ROOT else []))
        write(ROOT + "/etc/machine-id", uuid.uuid4().hex + "\n", 0o444)
        if req["entropy"] and not ROOT:
            with open("/dev/urandom", "wb") as f:
                f.write(req["entropy"])
        boot(seed)
        if not ROOT:
            grow_root()
            run("sh", seed + "/vmm-disks.sh")
            run("systemctl", "restart", "ssh.service")
    finally:
        umount_seed()


def ping(req):
    if not ROOT:
        # Exits non-zero when cloud-init finished with errors; the guest is up either way.
        subprocess.run(["cloud-init", "status", "--wait"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


OPS = {"ping": ping, "resume": resume, "takeoff": takeoff}


def parse(line):
    raw = json.loads(line)
    if not isinstance(raw, dict) or not isinstance(raw.get("op"), str):
        raise ValueError("bad request")
    t, macs, entropy = raw.get("time", 0), raw.get("macs") or [], raw.get("entropy") or ""
    if type(t) is not int or t < 0:
        raise ValueError("bad time")
    if not isinstance(macs, list) or len(macs) > 16 or not all(isinstance(m, str) and MAC.fullmatch(m) for m in macs):
        raise ValueError("bad macs")
    if not isinstance(entropy, str) or len(entropy) > 1024:
        raise ValueError("bad entropy")
    return {"op": raw["op"], "time": t, "macs": macs, "entropy": base64.b64decode(entropy, validate=True)}


def handle(conn):
    with conn:
        try:
            conn.settimeout(10)
            buf = b""
            while b"\n" not in buf:
                chunk = conn.recv(4096)
                if not chunk:
                    raise ValueError("incomplete request")
                if len(buf) + len(chunk) > MAX_REQUEST:
                    raise ValueError("request too large")
                buf += chunk
            req = parse(buf.split(b"\n", 1)[0])
            op = OPS.get(req["op"])
            if op is None:
                reply = {"ok": False, "error": "unknown op"}
            else:
                conn.settimeout(None)
                if op is ping:
                    op(req)
                else:
                    with lock:
                        op(req)
                reply = {"ok": True}
        except subprocess.CalledProcessError as e:
            err = e.stderr.decode(errors="replace") if isinstance(e.stderr, bytes) else str(e.stderr or "")
            reply = {"ok": False, "error": "%s failed: %s" % (e.cmd[0], err.strip()[-500:])}
        except Exception as e:
            reply = {"ok": False, "error": str(e)[:500] or type(e).__name__}
        try:
            conn.sendall(json.dumps(reply).encode() + b"\n")
        except OSError:
            pass
        finally:
            slots.release()


def main():
    path = os.environ.get("VMM_AGENT_UNIX")
    if path:
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.bind(path)
    else:
        s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
        s.bind((socket.VMADDR_CID_ANY, PORT))
    s.listen(8)
    while True:
        conn, addr = s.accept()
        # With vsock_loopback loaded, guest users could connect as CID 1 or 3; only the host (CID 2) may drive the agent.
        if (not path and addr[0] != socket.VMADDR_CID_HOST) or not slots.acquire(blocking=False):
            conn.close()
            continue
        threading.Thread(target=handle, args=(conn,), daemon=True).start()


if __name__ == "__main__":
    main()
