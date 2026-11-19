#!/usr/bin/python3
# Vsock agent for the node; requests never carry keys or variables (the seed does), so never log request values.
# Also run as "vmm-agent.py install-cas <file>" by vmm-boot.sh.
import base64
import concurrent.futures
import fcntl
import glob
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import threading
import time
import uuid

PORT = 1050
MAX_REQUEST = 64 << 10
ROOT = os.environ.get("VMM_ROOT", "")
SEED_DIR = os.environ.get("VMM_SEED_DIR", "")
SEED_MOUNT = "/run/vmm-seed"
NETPLAN_GENERATE = "/usr/libexec/netplan/generate"
RNDRESEEDCRNG = 0x5207
MAC = re.compile(r"[0-9a-f]{2}(:[0-9a-f]{2}){5}")
HOSTNAME = re.compile(r"[A-Za-z0-9]([A-Za-z0-9-]{0,62})")
CA_DIR = "/usr/local/share/ca-certificates"
CA_ID = re.compile(r"[0-9a-f-]{36}")
CA_PEM = re.compile(r"(-----BEGIN CERTIFICATE-----\r?\n[A-Za-z0-9+/=\r\n]+-----END CERTIFICATE-----\r?\n?)+")
MAX_CA = 16 << 10
UPDATE_CA = os.environ.get("VMM_UPDATE_CA", "" if ROOT else "update-ca-certificates")
CA_PENDING = ROOT + "/etc/vmm/ca-update-pending"

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


def reseed_rng(entropy):
    # A clone starts with its template's CRNG state; mix in host entropy and make the kernel rekey before anything random is made.
    if len(entropy) < 16:
        raise ValueError("takeoff needs host entropy")
    with open("/dev/urandom", "wb", buffering=0) as f:
        f.write(entropy)
        fcntl.ioctl(f, RNDRESEEDCRNG)


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
    # MACs first: anything the resumed guest sends (timers fire once the clock moves) must not carry the template's MAC.
    if not ROOT:
        set_macs(req["macs"])
    set_clock(req["time"])
    if not ROOT:
        reseed_rng(req["entropy"])
    seed = mount_seed()
    try:
        with open(seed + "/meta-data") as f:
            meta = f.read()
        m = re.search(r"^local-hostname:\s*(\S+)\s*$", meta, re.M)
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
        # Before the network: networkd derives DHCP client ids from the machine id.
        write(ROOT + "/etc/machine-id", uuid.uuid4().hex + "\n", 0o444)
        m = re.search(r"^instance-id:\s*(\S+)\s*$", meta, re.M)
        if m and HOSTNAME.fullmatch(m.group(1)):
            adopt_instance(m.group(1))
        # Keys before the network, so the template's host key never answers on the VM's address.
        new_host_keys()
        if ROOT:
            boot(seed)
            return
        run("systemctl", "restart", "ssh.service")
        apply_network()
        # growpart mostly waits for the kernel and udev, so it overlaps the rest.
        with concurrent.futures.ThreadPoolExecutor(1) as pool:
            grown = pool.submit(grow_root)
            run("sh", seed + "/vmm-disks.sh", env=dict(os.environ, VMM_AGENT="1"))
            grown.result()
        # Like vmm-boot.sh: a guest without the CAs still runs, only bound hosts fail TLS.
        try:
            install_cas(seed_cas(seed + "/vmm-egress-cas"))
        except Exception as e:
            print("install egress cas: %s" % e, file=sys.stderr)
        # A fork from a memory snapshot carries the template's integration files; refresh them now, not in up to 5 minutes.
        try:
            subprocess.run(["systemctl", "--no-block", "start", "vmm-integrations.service"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10)
        except (OSError, subprocess.SubprocessError):
            pass
    finally:
        umount_seed()


def adopt_instance(iid):
    # cloud-init would take the VM's instance id for a new instance on the next boot and make new host keys.
    cloud = ROOT + "/var/lib/cloud"
    link = cloud + "/instance"
    if not os.path.islink(link):
        return
    old = os.path.basename(os.readlink(link))
    if old != iid and os.path.isdir(cloud + "/instances/" + old):
        if os.path.exists(cloud + "/instances/" + iid):
            shutil.rmtree(cloud + "/instances/" + iid)
        os.rename(cloud + "/instances/" + old, cloud + "/instances/" + iid)
        # The cached datasource still names the old instance.
        for f in glob.glob(cloud + "/instances/" + iid + "/obj.pkl"):
            os.remove(f)
    if os.path.lexists(link + ".new"):
        os.remove(link + ".new")
    os.symlink("/var/lib/cloud/instances/" + iid, link + ".new")
    os.replace(link + ".new", link)
    os.makedirs(cloud + "/data", exist_ok=True)
    for name in ("instance-id", "previous-instance-id"):
        write(cloud + "/data/" + name, iid + "\n")


def apply_network():
    # netplan's own CLI costs about a second of CPU on a small VM; its generator plus networkd do the same for unrenamed NICs.
    if not os.path.exists(NETPLAN_GENERATE):
        run("netplan", "apply")
        return
    run(NETPLAN_GENERATE)
    run("networkctl", "reload")
    run("networkctl", "reconfigure", *nics())


def new_host_keys():
    for k in glob.glob(ROOT + "/etc/ssh/ssh_host_*"):
        os.remove(k)
    os.makedirs(ROOT + "/etc/ssh", exist_ok=True)
    if ROOT:
        run("ssh-keygen", "-A", "-f", ROOT)
        return
    for t in ("ed25519", "ecdsa"):
        run("ssh-keygen", "-q", "-t", t, "-N", "", "-f", "/etc/ssh/ssh_host_%s_key" % t)
    # An RSA key takes seconds of CPU on a small VM; sshd starts without it and reloads once it exists.
    subprocess.Popen(["nice", "-n", "19", "sh", "-c", "ssh-keygen -q -t rsa -N '' -f /etc/ssh/ssh_host_rsa_key && systemctl reload ssh.service"],
                     stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def install_cas(certs, force=False):
    # Only public CA certs may land in the trust store: a key or any other PEM block is refused.
    if not isinstance(certs, dict) or not all(
            isinstance(i, str) and CA_ID.fullmatch(i) and isinstance(p, str) and len(p) <= MAX_CA and CA_PEM.fullmatch(p)
            for i, p in certs.items()):
        raise ValueError("bad certs")
    d = ROOT + CA_DIR
    os.makedirs(d, exist_ok=True)
    changed = removed = False
    for i, pem in certs.items():
        path = "%s/observatory-egress-%s.crt" % (d, i)
        try:
            with open(path) as f:
                if f.read() == pem:
                    continue
        except FileNotFoundError:
            pass
        write(path, pem)
        changed = True
    for path in glob.glob(d + "/observatory-egress-*.crt"):
        if os.path.basename(path)[len("observatory-egress-"):-len(".crt")] not in certs:
            os.remove(path)
            changed = removed = True
    # A failed update leaves the files in place, so only the marker tells the next call to update again.
    pending = os.path.exists(CA_PENDING)
    if (changed or force or pending) and UPDATE_CA:
        os.makedirs(os.path.dirname(CA_PENDING), exist_ok=True)
        write(CA_PENDING, "")
        # Without additions it neither drops the stale hash link nor rehashes, and OpenSSL stops at the first missing hash link.
        run(UPDATE_CA, *(["--fresh"] if removed or force or pending else []))
        os.remove(CA_PENDING)
    return changed


def seed_cas(path):
    try:
        with open(path) as f:
            return json.load(f) or {}
    except FileNotFoundError:
        return {}


def trust(req):
    install_cas(req["certs"], force=True)


def ping(req):
    if not ROOT:
        # Exits non-zero when cloud-init finished with errors; the guest is up either way.
        subprocess.run(["cloud-init", "status", "--wait"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


OPS = {"ping": ping, "resume": resume, "takeoff": takeoff, "trust": trust}


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
    return {"op": raw["op"], "time": t, "macs": macs, "entropy": base64.b64decode(entropy, validate=True), "certs": raw.get("certs", {})}


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
    if sys.argv[1:2] == ["install-cas"]:
        install_cas(seed_cas(sys.argv[2]))
    else:
        main()
