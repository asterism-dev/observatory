# Guest experience (image, agent context, desktop, browser)

## Intent

Project 7 of the VM port (roadmap in `2026-11-17-vms-nodes-design.md`; projects 1–6 are built). Today a VM boots the plain Ubuntu 24.04 cloud image: the integrations from project 5 write a gitconfig, a `gh` wrapper, a Claude credentials file and a Linear MCP entry, but `gh`, Claude Code, Codex, Docker and Node are not there, and an agent that lands in the VM knows nothing about placeholders, the token service or auto-suspend. This project makes a fresh VM a ready workplace for a person and their coding agents: a guest image with the usual tools preinstalled, a short context file that tells agents how the platform works, and an on-demand desktop on which a browser runs visibly, so a person can watch and help an agent that drives it.

Success: an org admin adds a node; it builds the guest image in the background and the Nodes page shows `Guest image 2026.11.1-3f9a0c1e · ready`. A member creates a VM; in it `claude -p "say ok"`, `gh api user`, `codex --version`, `docker run --rm hello-world`, `node -v`, `uv run --with httpx python -c "import httpx"` work without any setup, and `claude mcp list` shows `linear`. Claude Code in the VM answers "how do I get the GitHub token?" with `vmm-token github` and knows that `obsph_…` is expected. The agent runs `agent-browser open https://example.com`; on the laptop `xpra attach ssh://ubuntu@dev.internal/100` shows that Chrome window, the member logs into a site by hand, and after hibernate, resume and a reboot the agent is still logged in. No port in the VM listens for the desktop or the browser on any network interface. After a tool bump the node builds the next image; existing VMs are untouched, new VMs get it, templates rebuild on their own.

## Decided with the user

- **Guest image** with Claude Code, Codex, `gh`, `git`, Docker, Node, Python and `uv` preinstalled.
- **Agent context file** (AGENTS.md / CLAUDE.md) in the guest describing the platform: how variables, secrets, integrations and the egress proxy work, `vmm-token`, and what not to do.
- **xpra desktop on demand**, bound to loopback only, reached through an authenticated tunnel (hub or SSH). An HTML viewer comes later.
- **`agent-browser` headed** on that desktop with a persistent profile.
- **No stealth browser.** A bridge to the laptop's own browser only as an explicit opt-in, and **deferred**. **No toolkit catalog.**
- **Project 5's integrations must work with the preinstalled tools:** `claude` is found in `PATH` by the Linear MCP sync; `gh` lives at `/usr/bin/gh` behind the wrapper.

Everything else is marked *(Our call.)*

## Goals

- **Guest image built on the node** from a pinned base cloud image and a pinned manifest of tools (version + SHA-256 each), versioned by the manifest's hash, used for new VMs and templates.
- **Tools system-wide** and owned by root, so the image can be rebuilt without touching home directories and a user's own installs (`~/.local`) win.
- **Docker that works in a Firecracker VM** out of the box, without clashing with the platform's address plan or slowing down boot.
- **Context file** for Claude Code and Codex, shipped with the seed (so it follows the node version, not the image), ≤ 8 KiB.
- **`vmm-desktop`**: xpra in seamless mode on display `:100` as the `ubuntu` user, started on demand, stopped when idle, reachable only through its Unix socket, i.e. through SSH over the existing device/WireGuard path.
- **`agent-browser` wrapper**: starts the desktop, runs Chrome headed on `:100` with one shared persistent profile.
- **Image state visible:** node page shows the image version and build state, VM page the image the VM was created from and how to open its desktop; de + en.

## Non-goals

- The HTML viewer and a hub-side desktop tunnel (later; see "Desktop": `--bind-vsock` is the intended path).
- The laptop-browser bridge (deferred opt-in), a stealth browser, computer-use APIs (screenshot/click from the hub), a toolkit or provider catalog.
- A Codex/OpenAI integration *(Our call: Codex is installed; the user runs `codex login --device-auth` themselves; an integration is a fourth kind in project 5's table later)*.
- Upgrading tools inside existing VMs. A VM's root disk is its own; the image is only the starting point *(Our call)*.
- Choosing an image per VM, an image catalog, custom images built by users *(Our call: one guest image per node; `imageURL` in `node.json` remains the escape hatch for a stock or custom image)*.
- Distributing images from the hub, OCI-based rootfs builds (see Approaches).
- The CLI command that opens the desktop (`observatory desktop <vm>`): project 8.
- Audio, printing and file transfer over xpra *(Our call: clipboard stays on)*.

## Anti-goals

- xpra, Chrome's DevTools port or any other desktop/browser endpoint listening on a VM network interface, or reachable by another UID or a container in the VM.
- A secret, key, token or user file in the image or in a template.
- An artifact in the image that was not checked against a pinned SHA-256 (or a pinned apt repository key and package version).
- The image or the seed overwriting a user's own files: their `~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md`, npm prefix, Chrome profile of their own.
- Docker networks that overlap the platform's ranges (`192.168.64.0/24` NAT, `10.64.0.0/10`, `10.192.0.0/16`, …) and cut a VM off from its gateway or devices.
- A context file that names the reference design's product or tells agents to work around the platform (disable the proxy, print tokens, overwrite the wrappers).

## Approaches

- **A) Build the image on each node inside a Firecracker build VM, from an embedded build script and manifest (picked).** The node already boots throwaway VMs for templates; an image build is the same: clone the pinned cloud image, boot it with a seed whose user-data runs `vmm-image-build.sh`, wait for it to power off, keep the disk. Installers run in a guest, never on the node; nothing new in the hub; works with today's "hub serves the node binary" distribution because script and manifest are embedded in the node binary. Cost: every node downloads ~1.5 GB and spends ~10 minutes once per image version, and images on two nodes are equivalent, not bit-identical.
- B) Build once (CI or a builder node) and distribute through the hub with a checksum. Bit-identical and fast to roll out, but needs a Linux build host with KVM in the release path, a place to store multi-GB images per version, and resumable transfers over the node connection. Worth it once there are many nodes; the manifest and script stay the same, only the producer moves.
- C) Dockerfile → rootfs (OCI), as in the reference design. Reproducible layering, but needs Docker on the build host, a converter to an ext4 disk with kernel, initrd and cloud-init, and postinst scripts that expect systemd do not run in `docker build`.
- D) Install at first boot via cloud-init per VM. Minutes per VM, breaks template fast boot, and every VM can end up with different versions.

## Guest image

- **Base:** the Ubuntu 24.04 cloud image pinned by serial (`releases/noble/release-<serial>/…`) with its SHA-256 in the manifest; `EnsureBaseImage` gains the checksum check (today it has none) *(Our call)*.
- **Manifest** `internal/node/guestimage/manifest.json` + `vmm-image-build.sh`, embedded in the node binary. **Image version** = `<manifest date>-<first 8 hex of SHA-256(manifest, build script, base checksum)>`, e.g. `2026.11.1-3f9a0c1e`. Written into the image as `/etc/observatory/image` *(Our call)*.
- **Apt reproducibility:** every apt call in the build uses Ubuntu's snapshot service with the manifest's timestamp (`apt-get --snapshot <stamp>`); third-party repositories (Docker, GitHub CLI) are added with the key fingerprint from the manifest and installed with exact package versions *(Our call)*.
- **Build:** the node clones the base image to the image size, boots a build VM (2 vCPU, 4 GiB, NAT tap, no keys, no variables, no egress proxy, no management NIC) whose user-data runs the build script, which downloads, verifies and installs, then `apt-get clean`, `cloud-init clean --logs --machine-id`, removes SSH host keys, `fstrim`, writes `/etc/observatory/image` last and powers off. The node accepts the disk only if Firecracker exited after a guest poweroff and `/etc/observatory/image` on the disk holds the expected version (read with the same disk reader as kernel extraction). Timeout 30 minutes; one build at a time, shares the template build lock *(Our call)*.
- **When:** at node start if the current version is missing, unless `imageURL` is set in `node.json` (then that image is used as is, as today). VMs created on the node while the image builds stay `starting` with the message "preparing guest image"; a failed build is retried after 30 minutes and its error shown on the node and on waiting VMs *(Our call: a VM silently falling back to the stock image would be a VM without its tools)*.
- **Size:** image virtual size **10 GiB** (≤ 5 GiB used). The API's minimum root disk and `templateDiskGB` go from 5 to 10 GB; existing 5 GB VMs keep running *(Our call: the tools do not fit into 5 GB with room to work)*. Free space required before a build: 2 × image size.
- **Update strategy:** a tool bump is a PR changing the manifest; a node running the new binary builds the new version in the background and switches new VMs to it once ready. Templates are keyed by the base image file, so the next VM of a size cold-boots once and builds its template from the new image; old templates age out (LRU, as today). The node keeps the current and the previous image (a template restore that falls back to `recloneBase` still finds its image) and deletes older ones *(Our call)*. Existing VMs are not changed; the context file and wrappers (seed) still update at their next boot or resume.
- **Reported:** the node sends `image {version, state: building|ready|failed, error, builtAt}` in `register`/`status`; each VM's `vm.json` records the image version it was cloned from and reports it with its state.

## Tools

| Tool | Source *(Our call)* | Installed at |
|---|---|---|
| git, curl, jq, ripgrep, build-essential, unzip, xterm, python3 (3.12) + venv | Ubuntu archive (snapshot) | `/usr/bin` |
| `gh` | GitHub CLI apt repository, exact version | `/usr/bin/gh` (behind project 5's `/usr/local/bin/gh` wrapper) |
| Docker Engine, containerd, buildx, compose plugin | Docker apt repository, exact versions | `/usr/bin` |
| Node.js (current LTS) | nodejs.org tarball + `SHASUMS256.txt` value pinned in the manifest | `/usr/local` |
| Claude Code | official release binary for `linux-x64`, version + SHA-256 from its release manifest | `/usr/local/bin/claude` |
| Codex | GitHub release `x86_64-unknown-linux-musl` binary, version + SHA-256 | `/usr/local/bin/codex` |
| `uv`, `uvx` | GitHub release tarball, version + SHA-256 | `/usr/local/bin` |
| `agent-browser` | npm, exact version, `npm ci`-style from a lockfile in `internal/node/guestimage/` | `/opt/vmm/agent-browser` (real binary; wrapper in `/usr/local/bin`) |
| Chrome for Testing | Google's CfT download, version + SHA-256 | `/opt/vmm/chrome` |
| xpra, Xvfb | xpra.org apt repository (Ubuntu's xpra is too old for the options below), exact version | `/usr/bin` |

- **Why Chrome for Testing:** Ubuntu ships Chromium only as a snap, and snapd in a Firecracker guest is slow to start and fragile; CfT is versioned, checksummable and what `agent-browser` drives anyway.
- **User installs win:** `/usr/local/etc/npmrc` sets `prefix=${HOME}/.local`, so `npm i -g` works without sudo and lands in `~/.local/bin`, which Ubuntu's `.profile` puts first in `PATH`. `claude update` / `claude install` install into `~/.local/bin` too. The image sets `DISABLE_AUTOUPDATER=1` (outside the `vmm` block of `/etc/environment`) so Claude Code does not nag about a root-owned install *(Our call)*.
- **Integrations (decided):** `claude` is at `/usr/local/bin`, which is in the default `PATH` of `vmm-integrations.service`, so the Linear MCP sync finds it; `gh` is `/usr/bin/gh`, which the wrapper execs. Nothing in project 5 changes. The credentials file written before Claude Code existed is picked up as is.
- **User `ubuntu`:** created by cloud-init at first boot as today; the image adds `docker` to `default_user.groups` in `/etc/cloud/cloud.cfg.d/` *(Our call: Docker group = root in the VM, and `ubuntu` has passwordless sudo anyway)*.

## Docker in the VM

- **Kernel:** the guest boots its own Ubuntu kernel (extracted from `/boot` per boot), so overlayfs, bridge, veth, `br_netfilter` and nftables are loadable modules. The build installs `linux-modules-extra` for the image's kernel if Docker's `check-config.sh` reports a missing required option, and the test asserts it reports none *(Our call)*.
- **cgroup v2** (noble's default, unified) and the `systemd` cgroup driver; **storage** `overlayfs` (containerd image store, Docker's default) on the ext4 root; no nested KVM (documented: no `/dev/kvm` in the guest).
- **Address pools:** `/etc/docker/daemon.json` sets `bip` `172.17.0.1/16` and `default-address-pools` `172.18.0.0/15` … `172.31.0.0/16` only. Docker's default pools include `192.168.0.0/16`, which would eventually overlap the NAT NIC `192.168.64.0/24` *(Our call)*.
- **Socket activation:** `docker.socket` enabled, `docker.service` and `containerd.service` not started at boot. Boot time, template memory and idle RAM stay as without Docker until the first `docker` call (~1 s extra on that call) *(Our call)*.
- **Egress:** containers leave through the VM's NAT, so the node's redirect applies; bound hosts present the VM's CA, which containers do not trust (project 4's known limit). The context file says so and how to mount the bundle (`-v /etc/ssl/certs:/etc/ssl/certs:ro` + `SSL_CERT_FILE`); integrations do not reach containers (project 5's known limit).

## Agent context file

- **Source:** `internal/node/guest/AGENTS.md`, embedded and shipped in the seed, installed by `vmm-boot.sh` at every boot, resume and takeoff to `/usr/local/share/vmm/AGENTS.md` (0644) *(Our call: it describes platform behaviour, which changes with the node, not the image)*.
- **Claude Code:** linked as `/etc/claude-code/CLAUDE.md`, the system-wide (managed) memory file, which Claude Code reads in addition to the user's own `~/.claude/CLAUDE.md` *(Our call: no file in the home directory to collide with; the builder verifies the path for the pinned version)*.
- **Codex:** `~/.codex/AGENTS.md` is written by the integrations sync only if missing or ours (hash bookkeeping as for the credentials file); a user's own file is never touched.
- **Contents** (≤ 8 KiB, plain facts, imperative where it matters):
  - What this machine is: an observatory VM (Firecracker), user `ubuntu` with sudo, reachable as `<vm>.internal` from the owner's devices; image version in `/etc/observatory/image`.
  - Variables: in `/etc/environment` and login shells, applied at boot; changes need a reboot, host-bound ones do not.
  - Secrets and integrations: values may be placeholders `obsph_…` — expected, do not "fix" or replace them; the egress proxy puts the real value into requests to the bound hosts. `vmm-token <github|claude|linear>`, git via HTTPS + credential helper (SSH remotes are rewritten), `gh` wrapper, Claude credentials file, Linear MCP. Never print, log, commit or copy a token or placeholder into files, issues or chat.
  - Do not: overwrite `/usr/local/bin/gh`, `/etc/gitconfig`'s vmm include, `~/.claude/.credentials.json`, the trust variables (`SSL_CERT_FILE` …); stop `vmm-agent` or `vmm-integrations`; run `claude /login` unless the user asks; set `credential.helper store`.
  - Egress: open internet, no allowlist; certificate errors on bound hosts mean a pinned or own trust store (containers, JVM).
  - Lifecycle: the VM may be paused, hibernated or auto-suspended on network idle — CPU-only jobs can be suspended; clocks jump and long-lived connections drop after resume; forks get a new hostname and IP.
  - Desktop and browser: `agent-browser` usage in three lines, that the user may be watching, that the profile is shared and logged-in sessions belong to the user; `vmm-desktop start|stop|status`; `DISPLAY=:100 <app> &` to show other windows.
  - Docker: socket-activated, address pools, CA mount for bound hosts.
  - Disks: attached disks are mounted at the paths shown on the VM page.
- **Test:** a unit test keeps it under 8 KiB and checks that every command and path it names exists in the guest scripts or the manifest.

## Desktop

- **`vmm-desktop.service`**, a **systemd user unit** of `ubuntu` (shipped in the seed, installed to `/etc/systemd/user/`, not enabled). `vmm-boot.sh` enables linger for `ubuntu` so the user manager runs without a login and the wrappers can start the unit without sudo *(Our call)*.
- **Command:** `xpra start :100 --daemon=no --bind=auto --bind-tcp= --mdns=no --html=off --pulseaudio=no --printing=no --file-transfer=off --notifications=no --clipboard=yes` with `Xvfb +extension GLX +extension Composite -screen 0 5120x2880x24+32 -nolisten tcp -dpi 96`, seamless mode. The large framebuffer is only the RandR maximum (xpra resizes to the client; ~56 MB), as the reference design found smaller ones leave unreachable areas *(Our call)*.
- **Access:** **no TCP listener**. xpra's socket lives in `$XDG_RUNTIME_DIR/xpra/` (0700, `ubuntu` only), so no other UID and no container can connect. From a device: `xpra attach ssh://ubuntu@<vm>.internal/100`; xpra's SSH mode runs `xpra _proxy` in the guest and talks to the socket. Authentication is the existing path: WireGuard device (only VMs the person may `vm:connect`, connect check unchanged) + SSH key *(Our call: no new listener, no new credential, no hub change)*. Stricter than "loopback only", which the decision allows.
- **Later (HTML viewer):** xpra's `--bind-vsock` on a fixed port gives the node a channel that no guest process or network peer can reach; the hub tunnels to it after `vm:connect`. Not built now.
- **`/usr/local/bin/vmm-desktop start|stop|status`** (seed): `systemctl --user start|stop|is-active`; `start` waits until the socket answers (≤ 5 s). Starting the desktop from a laptop: `ssh ubuntu@dev.internal vmm-desktop start && xpra attach ssh://ubuntu@dev.internal/100`.
- **Idle stop:** the desktop stops after 30 minutes with no xpra client attached and no window open (Chrome closed), checked every minute by a user timer via `xpra info` *(Our call: Xvfb + xpra cost ~150 MB, Chrome much more; a desktop someone forgot should not hold that, and an agent's open browser keeps it up)*.
- **Lifecycle:** hibernate/resume keep the desktop and windows; attached clients lose their SSH connection and re-attach. **Takeoff** (template or fork with memory) stops the desktop: a fork's Chrome would otherwise hold the source's profile lock and hostname *(Our call)*. A template never has a desktop running (it is built without starting it).
- **No guest agent op** in this project: nothing on the hub needs to start the desktop until the HTML viewer *(Our call)*.

## Browser

- **Wrapper `/usr/local/bin/agent-browser`** (seed): `vmm-desktop start` if needed, then exec `/opt/vmm/agent-browser/bin/agent-browser` with `DISPLAY=:100`, headed, Chrome at `/opt/vmm/chrome/chrome`, session `default`, user data dir `~/.local/share/vmm-browser/default`, `--password-store=basic` (no keyring daemon needed for logins to stick). The builder maps these to the pinned version's flags/variables *(Our call: one shared session and profile, so a login done by hand on the desktop is the agent's login too, and persistence comes from Chrome's profile, not from state save/restore — the reference design found the latter produced phantom tabs and lost logins)*.
- **DevTools:** `agent-browser` connects to Chrome over a pipe or a loopback port it picks; the wrapper refuses `--remote-debugging-address` other than `127.0.0.1` and `--cdp` URLs that are not loopback *(Our call: this is also where a future opt-in laptop bridge would be allowed, explicitly)*.
- **Profile persistence:** the profile is on the root disk, so it survives reboot, hibernate (memory + disk), auto-suspend, snapshots and restore; forks (same owner by rule) start with the same logins. Templates never contain a profile. Before starting Chrome the wrapper removes `SingletonLock`/`SingletonSocket`/`SingletonCookie` when no Chrome of this user runs and the lock names another hostname or a dead PID (after a disk-only restore or a fork) *(Our call)*.
- **Respecting the user's setup:** `AGENT_BROWSER_*` variables the user sets win over the wrapper's defaults; the wrapper is overwritten at every boot, so a different `agent-browser` belongs in `~/.local/bin`.
- **RAM:** documented guidance: desktop + browser want ≥ 4 GB; the API minimum (512 MB) is unchanged.

## Templates and resources

- Templates are keyed by the base image file (path, size, mtime), so a new image version means new templates; nothing else in the template code changes besides `templateDiskGB` = 10.
- **Budgets** (checked in the real-hardware run, recorded in the PR; a miss is a finding, not a test failure) *(Our call)*:

| | Budget |
|---|---|
| Image used size | ≤ 5 GiB (virtual 10 GiB) |
| Image build on a test node | ≤ 15 min |
| Cold boot to SSH | ≤ stock image + 3 s |
| Template boot (second VM of a size) | < 1 s, as today |
| Idle guest RAM after boot (no Docker use, no desktop) | ≤ stock image + 100 MB |
| `vmm-desktop start` | ≤ 5 s; first `agent-browser open` ≤ 10 s |

## Security

- Build VMs get no keys, no variables, no integrations and no management NIC; all downloads are verified inside the build against the manifest; a failed check fails the build. The image holds no secret by construction, like templates.
- xpra and Chrome are reachable only by `ubuntu` processes in the VM (Unix socket 0700, loopback CDP). Anyone who can SSH as `ubuntu` — i.e. passes the connect check — can watch and drive the desktop, as they can already read the user's files.
- The shared browser profile holds the user's web sessions: every process in the VM, every snapshot and fork can use them. The context file and docs say so; with the integration's inside-VM mode it is the same class of risk.
- Docker group membership is root-equivalent inside the VM; no change to the node's trust boundary.

## Permissions

- No new actions. The desktop is reached over SSH, so `vm:connect` and the connect check govern it. Image version and build state of a node are visible with `node:read`; a VM's image with `vm:read`.

## UI

- **Node detail:** "Guest image" line: version, state (`building` with elapsed time, `ready` with build time, `failed` with the error and next retry), or "custom image (`imageURL`)".
- **VM detail:** "Image" line (the version the VM was created from, "stock" for older VMs); "Desktop" line for VMs with the guest image: `xpra attach ssh://ubuntu@<vm>.internal/100` with a copy button and a link to the docs (xpra client install). VMs waiting for the image show "preparing guest image".
- **Create dialog:** minimum root disk 10 GB; hint "Desktop and browser want 4 GB RAM or more". de + en.

```
dev   running   Image 2026.11.1-3f9a0c1e
Desktop   xpra attach ssh://ubuntu@dev.internal/100   [Copy]   How to install xpra
```

## Left to the builder

- Exact manifest schema and the bump workflow (manual edit is fine; a `scripts/guest-image-bump.sh` that fetches current versions and checksums is optional).
- How the build VM signals completion beyond poweroff + version file; the build's disk reader for ext4 root; sharing the template build lock.
- Exact `agent-browser` flags/variables, Chrome path handling and the Linux path of Claude Code's managed memory file for the pinned versions.
- Whether `xpra info` or xpra's own idle options implement the idle stop.
- Test strategy; must include: manifest validation (every artifact has https URL, version, SHA-256; apt entries have key fingerprint and exact version); image version changes with manifest, script or base checksum; base image checksum mismatch refuses the image; template key changes with the image; VMs wait in "preparing guest image" and never fall back to the stock image; previous image kept, older deleted; `vmm-boot.sh` shell tests (PoC style, as `integrations_test.go`) for context file install, linger, user unit and wrappers; wrapper tests with fake binaries (desktop started, `DISPLAY` set, user's `AGENT_BROWSER_*` win, non-loopback CDP refused, stale lock removed only when no Chrome runs and hostname/PID differ); Codex `AGENTS.md` written only when missing or ours; context file ≤ 8 KiB and its named paths exist; Docker `daemon.json` pools disjoint from every range in the network spec's address plan.
- **Real-hardware run on Proxmox** (test VM 108 as node of org A, 107 as node of org B; hub on the Mac over the tailnet; a device on the Mac for `*.internal`; dummy integration credentials only): image build on 108 (time, used size, version on the Nodes page); cold boot and template boot times, idle RAM vs. the stock image; in a VM: every tool's version equals the manifest, `docker run --rm hello-world`, a compose project with three networks gets only `172.x` addresses, `check-config.sh` clean; project 5's dummy checks with the real `claude` (`claude mcp list` shows `linear`, credentials file present, `vmm-token github` → `obsph_…`, `gh` resolves to the wrapper then `/usr/bin/gh`), `scripts/integrations-check.sh` passing except the provider-auth lines; Claude Code shows the context file (`/memory` or asking it about `vmm-token`); `ss -ltnp` shows nothing from xpra or Chrome on non-loopback addresses; another UID and a container cannot open the xpra socket; from the Mac `xpra attach ssh://ubuntu@dev.internal/100` shows the window of `agent-browser open https://example.com`; a cookie set via `https://httpbin.org/cookies/set?k=v` survives hibernate/resume, reboot, disk-only snapshot restore, and a fork with memory (desktop stopped by takeoff, lock cleaned, cookie present); the idle stop after 30 minutes (shortened by a test override); a manifest bump on 108 builds the next image while existing VMs keep theirs, the next VM gets the new version and its template rebuilds; 107 never builds or serves anything for org A.
- Add `docs/guest.md` (image contents and versions, update strategy, Docker notes, desktop and browser usage incl. installing the xpra client on macOS/Linux, the context file, known limits: no nested KVM, containers and bound hosts, shared browser profile in snapshots and forks, `DISABLE_AUTOUPDATER`); update `docs/nodes.md` (image build, `imageURL`, disk space) and `docs/integrations.md` (remove the "until project 7" notes).
