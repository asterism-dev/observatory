# Guest image, desktop and browser

Every VM boots an OCI image on the platform's own kernel and init. The standard image is a Dockerfile in this
repository, built by the release pipeline from a pinned Ubuntu base and a pinned list of tools, so a fresh VM is a
ready workplace for a person and their coding agents: `claude`, `codex`, `gh`, Docker, Node, Python and `uv` work
without setup, the [integrations](integrations.md) find them, a context file tells agents how the platform works, and
an on-demand desktop shows a browser the agent drives. Nothing has to be inside an image: the init brings SSH, the
agent, the integrations and the egress CA.

```
ssh dev@dev.internal vmm-desktop start && xpra attach ssh://dev@dev.internal/100
```

You log in as `dev` (uid 1000, home `/home/dev`, passwordless `sudo` if the image has it). Root cannot log in over SSH.

## Contents

| Tool | Version | Source | Installed at |
|---|---|---|---|
| Ubuntu 24.04 | pinned by digest | Docker Hub | base |
| Ubuntu packages: git, curl, jq, ripgrep, build-essential, unzip, xterm, python3 (3.12) + venv, Xvfb, Chrome's libraries | snapshot `20261117T000000Z` | Ubuntu archive via `snapshot.ubuntu.com` | `/usr/bin` |
| `gh` | 2.105.0 | GitHub release tarball, SHA-256 | `/usr/bin/gh` (behind the `gh` wrapper in `/.observatory/bin`) |
| Docker Engine, CLI, containerd, buildx, compose | 29.10.0, 2.4.2, 0.39.0, 5.8.0 | Docker apt repository, key fingerprint + exact versions | `/usr/bin` |
| xpra (server, client, codecs, X11) | 6.5.5 | xpra.org apt repository, key fingerprint + exact versions | `/usr/bin` |
| Node.js (LTS) | 26.13.1 | nodejs.org tarball, SHA-256 | `/usr/local` |
| Claude Code | 2.1.311 | release binary `linux-x64`, SHA-256 | `/usr/local/bin/claude` |
| Codex | 0.170.0 | GitHub release `x86_64-unknown-linux-musl`, SHA-256 | `/usr/local/bin/codex` |
| `uv`, `uvx` | 0.13.9 | GitHub release tarball, SHA-256 | `/usr/local/bin` |
| Chrome for Testing | 158.0.8127.36 | Google's CfT `linux64` zip, SHA-256 | `/opt/vmm/chrome/chrome` |
| `agent-browser` | 0.43.0 | npm, `npm ci` from a lockfile (sha512 per package) | `/opt/vmm/agent-browser` (wrapper in `/.observatory/bin`) |

The source of truth is `deploy/guest.Dockerfile`; this table follows it. The image also contains:

- `/usr/local/etc/npmrc` with `prefix=${HOME}/.local`: `npm i -g` works without sudo and lands in `~/.local/bin`,
  which Ubuntu's `~/.profile` puts first in `PATH` (after the platform helpers), so your own installs win over the image's.
- `DISABLE_AUTOUPDATER=1` in `/etc/environment`: Claude Code does not try to update its root-owned binary.
  `claude install` puts a version of your own into `~/.local/bin`.
- `/etc/docker/daemon.json` and the Docker settings below; the user `dev` (the init renames the base image's `ubuntu`, uid 1000) is in the `docker` group.

Nothing in the image holds a secret, key, token or user file; the init writes them into a VM at boot.

## Version

The image is `ghcr.io/asterism-dev/observatory-guest`, tagged with the release version and `latest`; a node pulls the
tag of its own version (`latest` for an unreleased build). The VM page and `observatory vm show` show the reference
and digest a VM was created from (`ref@sha256:…`). Any change to a pin in the Dockerfile ships with the next release.

## Pull on the node

At start the node pulls the image for `linux/amd64`, flattens its layers into `images/<digest>.raw` (an ext4 file
without a partition table) and keeps layers by digest. There is no build VM. Details and failures:
[nodes.md](nodes.md#guest-image).

- The node page shows `Guest image <ref> · pulling image`, then `ready`, or `failed` with the error and the next retry.
- New VMs wait in `starting` with "preparing guest image" until the image is ready.

## Updates

- A tool bump is a change to `deploy/guest.Dockerfile`. A node on the new release pulls the new image and gives it to
  new VMs once it is ready.
- **Existing VMs are not changed**: a VM's root disk is its own, the image is only its starting point. Update tools in
  a VM with apt or by installing into `~/.local/bin`, or create a new VM.
- Templates are keyed by the image digest, so the first VM of each size after a bump cold-boots and builds a new
  template; old templates age out.
- The context file, `vmm-desktop`, `agent-browser` and the other helpers come with the init, not with the image: a VM
  gets new ones at its next cold boot.

### Bump checklist

Research each pin from its upstream source, never from a mirror or a guess, and note source and date in the PR:

- [ ] **Base:** the `ubuntu:24.04` digest from Docker Hub.
- [ ] **`APT_SNAPSHOT`:** a recent `YYYYMMDDTHHMMSSZ`;
  `curl -fsI https://snapshot.ubuntu.com/ubuntu/<stamp>/dists/noble/InRelease` returns 200.
- [ ] **Docker:** key fingerprint of `https://download.docker.com/linux/ubuntu/gpg` (`gpg --show-keys`, cross-checked
  with Docker's install docs); `docker-ce`, `docker-ce-cli`, `containerd.io`, `docker-buildx-plugin`,
  `docker-compose-plugin` versions from `apt-cache madison` in an `ubuntu:24.04` container with that repository.
- [ ] **xpra:** key and fingerprint from xpra's Linux download page (`https://xpra.org/xpra.asc`, cross-checked), all
  `xpra*` packages at one exact version from `apt-cache madison`.
- [ ] **gh:** newest `cli/cli` release, `gh_<v>_linux_amd64.tar.gz`, SHA-256 from the release's checksums file,
  confirmed by download.
- [ ] **Node.js:** newest active LTS from `https://nodejs.org/dist/index.json`; SHA-256 of
  `node-v<v>-linux-x64.tar.xz` from `SHASUMS256.txt`, verified with `SHASUMS256.txt.sig` and the Node release keys.
- [ ] **Claude Code:** the `stable` version from the official installer's release bucket;
  `<bucket>/<version>/manifest.json` → `platforms["linux-x64"].checksum`, confirmed by download. Check that the
  version still reads `/etc/claude-code/CLAUDE.md` as managed memory.
- [ ] **Codex:** newest `openai/codex` release tag (pin the tag), asset `codex-x86_64-unknown-linux-musl.tar.gz`,
  SHA-256 from the asset's digest, confirmed by download.
- [ ] **uv:** `astral-sh/uv` release, `uv-x86_64-unknown-linux-gnu.tar.gz` and its `.sha256`.
- [ ] **Chrome for Testing:** Stable `linux64` `chrome` zip from
  `https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json`. Google
  publishes no checksum: download it, `sha256sum` it and pin that (trust on first pin), ideally comparing downloads
  from two networks.
- [ ] **`agent-browser`:** `npm view agent-browser version`; exact version in
  `deploy/guest/agent-browser/package.json`, then `npm install --package-lock-only` for the lockfile. Check that the
  wrapper's `AGENT_BROWSER_*` names and value flags still match the new version.
- [ ] **Ubuntu packages:** for a new Chrome, run `ldd chrome | grep 'not found'` in a noble container and compare with
  CfT's `deb.deps`.
- [ ] Build the image (`docker build -f deploy/guest.Dockerfile .`), run it on a test node and check the tools in a
  new VM.

## Own images

To keep, build and share images in observatory, see [images.md](images.md). Any `linux/amd64` OCI image boots as a VM;
set it per node with `"firecracker": {"guestImage": "<ref>"}` in `node.json` ([nodes.md](nodes.md#guest-image)). Build
and try it locally with `docker build` and `docker run`. What the platform does:

- **Nothing is required inside:** no SSH server, systemd, shell, Python or cloud-init. The init runs the image in its
  own PID and mount namespace and serves SSH, the agent and the integrations from outside it.
- **Login:** always `dev`. If the image has a user with uid 1000 (`ubuntu`, `node`), the init renames it to `dev`.
  The shell comes from `/etc/passwd`, else `/bin/sh`; without one SSH offers only port forwarding and SFTP. `USER` and
  `EXPOSE` of the image are ignored.
- **SFTP:** the init's SSH server serves the `sftp` subsystem itself, so `sftp dev@<vm>.internal`, `scp` (OpenSSH 9+
  uses SFTP) and the remote file access of VS Code or Zed work in every image, also without a shell or `sftp-server`.
  It runs inside the image as `dev`, so the usual file permissions apply. Legacy `scp -O` needs a shell and `scp` in
  the image.
- **Services:** an image with systemd runs it as init and `CMD` is ignored; otherwise `ENTRYPOINT` and `CMD` run as a
  supervised service as root and restart when they exit (log in `/var/log/observatory/cmd.log`).
- **Platform files:** the helpers (`vmm-token`, `git-credential-vmm`, `gh`, `vmm-desktop`, `agent-browser`) are in
  `/.observatory/bin`, read-only and first in `PATH`; the context file is
  `/usr/local/share/observatory/AGENTS.md`. The `gh` wrapper runs the image's `gh` if there is one.
- **Desktop and browser** work only if the image has xpra (and Chrome for `agent-browser`); otherwise the commands say
  so and exit non-zero.
- **Root disk:** the image grown to the VM's size; a VM cannot be smaller than its image.

## Docker

- **Socket activation:** `docker.socket` is enabled, `docker.service` and `containerd.service` are not started at
  boot. Boot time, template memory and idle RAM stay as without Docker; the first `docker` call starts the daemon (a
  few seconds).
- **Address pools:** `bip` `172.17.0.1/16`, `default-address-pools` `172.18.0.0/15` … `172.31.0.0/16`. Docker's
  defaults include `192.168.0.0/16`, which would overlap the VM's NAT network and cut it off from its gateway.
- **Storage:** overlayfs on the ext4 root, cgroup v2 with the systemd driver.
- **Bound hosts:** containers leave through the VM's NAT, so the egress proxy applies, but they do not trust the VM's
  CA. Mount the VM's store:
  `docker run -v /etc/ssl/certs:/etc/ssl/certs:ro -e SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt …`.
  Containers get no credential helper, `gh` wrapper or token service ([integrations.md](integrations.md#known-limits)).
- **No nested KVM:** nested virtualization is not supported (see Known limits).

## Desktop

`vmm-desktop` runs an xpra server in seamless mode on display `:100` as `dev`: each window appears as a window on
your laptop.

```
ssh dev@<vm>.internal vmm-desktop start && xpra attach ssh://dev@<vm>.internal/100
```

`observatory desktop <vm>` does both steps ([cli.md](cli.md#connecting)).

- **No network listener:** xpra listens only on Unix sockets in `/run/user/1000/xpra/` and `~/.xpra/` (sockets 0600,
  `dev` only); the X server has no TCP or abstract socket and requires an X authority
  (`/home/dev/.Xauthority`). `xpra attach ssh://…` logs in over SSH and talks to that socket, so access is
  exactly SSH access: a device of yours over WireGuard, the VM's connect check and your SSH key. Root in the VM, and
  so a container started with a bind mount of `/run/user/1000`, can reach the socket too (the `docker` group is
  root-equivalent).
- **Commands** (as `dev`, not with sudo): `vmm-desktop start` (waits up to 5 s until the desktop answers),
  `vmm-desktop stop`, `vmm-desktop status` (`active`, or `inactive` after a stop).
  `DISPLAY=:100 <app> &` shows any other window, e.g. `xterm`.
- **Process:** the init starts and stops xpra itself, on demand, as `dev` inside the image; there are no systemd units
  to enable.
- **Idle stop:** the init checks every minute and stops the desktop after 30 minutes with no client attached and no
  window open. An open browser keeps it running.
- **Lifecycle:** hibernate and resume keep the desktop and its windows; an attached client loses its SSH connection
  and attaches again. A fork or a template boot (takeoff) stops the desktop.
- **Clipboard** is on; audio, printing, file transfer and notifications are off. There is no HTML viewer yet.

### Installing the xpra client

Use an xpra 6.x client.

- **macOS:** the signed `.dmg` from `https://xpra.org` (Download → macOS), or `brew install --cask xpra`. The
  command-line client is `/Applications/Xpra.app/Contents/MacOS/Xpra`; for `xpra attach …` as above, link it into
  your `PATH` or use the full path.
- **Linux (Debian, Ubuntu, Fedora):** add xpra.org's repository as described on its Linux download page and install
  `xpra` (or `xpra-client-gtk3`). Distribution packages are often older than 6.x.

The SSH part uses your usual SSH client and keys; options go into `~/.ssh/config` for `*.internal`.

## Browser

`agent-browser` in the VM is a wrapper (`/.observatory/bin/agent-browser`) around the pinned
`/opt/vmm/agent-browser/bin/agent-browser`. It starts the desktop if needed and runs Chrome for Testing headed on
`:100`, so you can watch and take over:

```
agent-browser open https://example.com
agent-browser snapshot
agent-browser click @e1
```

- **One shared, persistent profile:** session `default`, user data dir `~/.local/share/vmm-browser/default`,
  `--password-store=basic` (no keyring needed for logins to stick). A login you do by hand on the desktop is the
  agent's login too. The profile is on the root disk, so it survives reboot, hibernate, auto-suspend, snapshots and
  restore, and forks start with the same logins. Templates never contain a profile.
- **Stale locks:** after a fork or a disk-only restore Chrome's `SingletonLock` names another host or a dead PID; the
  wrapper removes the lock files when no Chrome of yours runs.
- **Loopback only:** the wrapper refuses `--cdp` endpoints, `connect` targets and `--remote-debugging-address` values
  that are not loopback. DevTools never listens on a network interface, but it is a loopback TCP port: any process
  in the VM can reach it, other UIDs and host-network containers included. The VM is single-user, so this is
  `dev`'s own browser to drive; do not run untrusted code as another user in a VM with a logged-in browser.
- **Your settings win:** `AGENT_BROWSER_*` variables you set (e.g. `AGENT_BROWSER_HEADED=0`,
  `AGENT_BROWSER_PROFILE`) override the wrapper's defaults. The wrapper is part of the read-only platform directory;
  call a different `agent-browser` by its path.
- Run it as `dev`; `sudo agent-browser` and `sudo vmm-desktop` refuse.

## Context file

Agents in the VM get a short description of the platform: what the VM is, variables, placeholders (`obsph_…`) and the
egress proxy, `vmm-token`, what not to touch, lifecycle (suspend, forks), desktop and browser, Docker, disks and
limits. It is `internal/node/guestcontext/AGENTS.md` (at most 8 KiB), shipped with the init and written at every boot,
resume and takeoff:

| Path | For |
|---|---|
| `/usr/local/share/observatory/AGENTS.md` | the file itself (0644) |
| `/etc/claude-code/CLAUDE.md` | Claude Code's managed memory, a link to the file above; read in addition to your `~/.claude/CLAUDE.md`. A file or link an admin put there is kept. |
| `~/.codex/AGENTS.md` | Codex; written by the integrations sync only if missing or still the one it wrote; your own file is never touched. |

## Known limits

- **No nested KVM:** nested virtualization is not supported. Depending on the node's CPU, `/dev/kvm` may exist in
  the guest (root and group `kvm`), but VMs and emulators that need it are untested.
- **Containers and bound hosts:** containers neither trust the VM's CA nor get integrations; see Docker above.
- **Shared browser profile:** every process in the VM, and every snapshot and fork of it, can use the web sessions in
  the profile. Treat a fork as holding your logins.
- **`DISABLE_AUTOUPDATER`:** the preinstalled Claude Code does not update itself; it changes with the image (new VMs)
  or with `claude install` into `~/.local/bin`.
- **Codex login:** there is no Codex integration; run `codex login --device-auth` in the VM once.
- **RAM:** desktop and browser want 4 GB RAM or more; the API minimum stays 512 MB. Xvfb and xpra take about 150 MB,
  Chrome several hundred MB more; in a 1 GB VM the kernel's OOM killer ends the desktop once a viewer is attached
  and a few pages are open.
- **Root disk:** at least 10 GB (the image's files take about 3.7 GB of it).
- **Existing VMs** keep the tools they were created with.
- **Old runtime:** VMs created before the Go init are marked "Old runtime – recreate" and cannot be started again
  ([nodes.md](nodes.md#guest-image)).
