# Guest runtime (platform kernel, Go init, OCI root filesystems)

## Intent

Today every node builds the guest image itself: it boots a throwaway Firecracker VM from the Ubuntu cloud image, runs `vmm-image-build.sh` and keeps the result (`internal/provider/firecracker/guestimage_linux.go`). That takes 10–15 minutes per node and per image version. The guest side also depends on what is inside the image: the kernel is read from the guest disk (`boot.go`), setup runs through cloud-init and netplan, and the agent, integrations and browser wrapper are Python scripts started by systemd units. This rules out lightweight images (Alpine, Debian-slim, distroless) and custom images built by users.

The user wants their own images: every user has different tooling, some want lightweight images, and Docker is how they want to build them, because everyone knows a Dockerfile and can test the image locally with `docker run` before it becomes a VM. Images are built once, stored centrally, and nodes only fetch them.

That is two projects:

- **A – guest runtime (this document):** the platform supplies the kernel and an init that carries the agent, the integrations and an SSH server, so any OCI image boots as a VM with nothing required inside it. The standard image becomes a Dockerfile published on GHCR. Nodes pull it and turn it into a root disk instead of building it.
- **B – images as a resource (next design):** a central OCI registry run by observatory, image builds from an uploaded Dockerfile and context in a throwaway builder VM on a node (with the user's integrations and secrets), `docker push` from the CLI, versions and tags, org/team/private visibility, UI.

B depends on A, because a Docker image has no kernel and no init.

Success for A: a node joins and has no image build step; the first VM on it takes as long as a pull plus a minute, not 10–15 minutes. A VM created from the standard image behaves as today for the user: `observatory connect` lands as `dev`, `claude`, `codex`, `gh`, `docker`, `node`, `uv` work, integrations sync, the egress CA is trusted, the desktop and `agent-browser` work, fast boot from a template snapshot, hibernate/resume, snapshots and forks work. A VM created from `alpine:3` (a test case in the KVM test harness only, not reachable through the API in A — *Our call*; per-VM image choice comes with B) boots, accepts `observatory connect` as `dev` with `/bin/sh`, trusts the egress CA, and runs the image's `CMD` as a supervised service.

## Decided with the user

- **Docker/OCI images as the source of root filesystems.** Users build and test locally with `docker build` / `docker run`.
- **Nothing is required inside an image.** Agent, integrations and SSH access are platform guarantees. The desktop is optional: it works when the image contains xpra.
- **The agent and the integrations move to Go.** No Python in the guest path.
- **SSH server built into the init binary.** SFTP is wanted as a feature later; the design must leave room for it.
- **Login user renamed from `ubuntu` to `dev`.**
- **No root login over SSH** for now; root only via `sudo` inside the image.
- **Clean cut:** the old boot path (cloud-init, netplan, partitioned Ubuntu disk, kernel from the guest disk, Python scripts, image build on the node) is removed. Existing VMs are recreated, not migrated. Data on separate disks survives as usual.
- **`CMD`/`ENTRYPOINT` of the image runs as a supervised service** (restarted when it exits). For images with systemd, systemd is the init and `CMD` is ignored.
- **Init architecture (approach 1 below):** the init lives in an initramfs, stays PID 1 outside, and runs the image in its own PID namespace.
- **Order:** A first, then B. Central storage in B is an OCI registry (nodes pull layers and build the disk), not a central finished disk.
- **Image versions** (for B): every build/push is an immutable version (digest); tags such as `latest`, `1.2` are movable names; a VM records the digest it was created from; old versions can be deleted at any time because each VM owns its root disk; an existing VM is not upgraded in place.
- **Disk size:** the stored image is only a template. The VM's root disk is the image grown to the requested size; growing later works the same way. A VM cannot be smaller than its image.

Everything else is marked *(Our call.)*

## Goals

- **Platform kernel**, built from source in the release pipeline, monolithic (no modules), Firecracker device profile (virtio-mmio blk/net/vsock/balloon, serial console), plus everything Docker inside the VM needs (overlayfs, netfilter/nftables and iptables compat, bridge, veth, cgroups v2, user and PID namespaces, seccomp) and what the existing features need (vsock, virtio-balloon stats, ext4, NBD is host-side only). x86_64 only, like nodes today.
- **`observatory-init`**: one static Go binary in an initramfs. It is PID 1, sets up the VM, runs the image in a PID + mount namespace, and serves the agent protocol, the integrations and SSH from outside that namespace.
- **Root disk = ext4 without a partition table**, made from the image's flattened layers, grown to the VM's size.
- **Nodes pull OCI images and build root disks**, cached per digest. In A the only image is the standard image from GHCR (public, no credentials); B adds the observatory registry and private images.
- **Standard image as a Dockerfile** in the repo, built and pushed by the release pipeline like hub/gateway/storage. It replaces `manifest.json` + `vmm-image-build.sh` and keeps their pinning (versions + SHA-256 per download, apt snapshot).
- **No behaviour lost** for a VM from the standard image: SSH keys, env variables and secrets, egress CA, integrations (git credential helper, `gh` wrapper, `vmm-token`, Claude credentials, Linear MCP, Codex AGENTS.md), context file, attached disks, ready marker, fast boot/takeoff, hibernate/resume, snapshots/forks, auto-suspend, desktop + `agent-browser`.

## Non-goals

- Everything in project B: registry, builder VM, user images, upload, tags/versions UI, visibility, private registries.
- SFTP (later; the SSH server's subsystem handling must make it a small addition).
- arm64 nodes or guests.
- Upgrading the root disk of an existing VM to a newer image.
- Migrating existing VMs to the new boot path.
- Images for other architectures than linux/amd64 (the node rejects them).

## Approaches

How the init and the image fit together:

1. **Init in the initramfs, stays PID 1 outside; the image runs in its own PID + mount namespace** *(chosen)*. One path for images with and without systemd. Platform services cannot be stopped from inside the image, nothing of ours lives on the user's disk, a platform update is a new initramfs. Cost: systemd sees a container (`systemd-detect-virt` reports one), and SSH sessions must enter the image's namespaces.
2. **Init in the initramfs sets up, then `switch_root` + `exec` of the image's init.** We disappear after boot; agent, SSH and integrations would be systemd units copied onto the disk, and images without systemd still need our supervisor. Two paths, our files on the user's disk, our services stoppable by the user.
3. **No initramfs: copy the binary onto the root disk at clone time, `init=/.observatory/init`.** Simplest boot, but our files are on the user's disk and editable, and updates need write access to every disk. Same two paths as 2.

Approach 1 is the only one where agent, integrations and SSH stay a platform guarantee independent of the image, with one code path for every image.

Central storage (decided now, built in B): an OCI registry, nodes pull layers and build the disk locally, chosen over a central finished ext4 disk because user images typically share a large base image, so shared layers are transferred and stored once, and the CLI path is plain `docker push`.

## Design

### Boot

The node starts Firecracker with the platform kernel, the platform initramfs and the VM's `root.img` as `/dev/vda` (plus the seed and attached disks as today). Kernel and initramfs are version-locked to the node binary *(Our call: how they reach the node — embedded in the binary or served by the hub next to `observatory-node` with `SHA256SUMS` like today's installer — is left to the builder; there must be no separate install step and no version skew between node and init)*.

`observatory-init` as PID 1:

1. Mounts `/proc`, `/sys`, `/dev`, cgroup2, reads the seed (same seed drive as today, new content, see "Seed").
2. Grows the root filesystem to the disk size (`resize2fs`, online after mount or offline before — builder's call), mounts it.
3. Sets hostname, machine-id, static network config for every NIC from the seed (no DHCP client of the image is involved; *Our call*: the init configures addresses and routes itself via netlink, as the primary NIC already uses a static address today and OVN NICs get theirs from OVN DHCP — the init runs a minimal DHCP client for those, or the hub hands the OVN addresses through the seed; builder's call, the behaviour of `CloudInit`'s network-config must be preserved).
4. Generates SSH host keys (kept on the root disk under a platform path, regenerated on takeoff).
5. Prepares the login user `dev` (see "User"), env variables, egress CA, integration glue, attached disks (format if blank, mount, chown — the logic of `DiskScript`, including the blank check).
6. Starts its own services: agent (vsock 1050), integrations sync, SSH server.
7. Starts the image in a new PID + mount namespace with root on `/dev/vda`: `/sbin/init` (or `/lib/systemd/systemd`) if the image has systemd, otherwise the image's `ENTRYPOINT` + `CMD` as a supervised service (see "Image config"). An image with neither still boots; the init just reaps.
8. Prints the ready marker on the console once the primary NIC has its address (as `/run/vmm-ready.sh` does today).

The binaries the init needs that are not Go (`mkfs.ext4`, `resize2fs`, `blkid`-equivalent) live statically in the initramfs, or are replaced by Go code *(builder's call)*.

### What the image sees

- A bind-mounted, read-only platform directory (e.g. `/.observatory`, name is the builder's call) with the multi-call helpers: `vmm-token`, `git-credential-vmm`, the `gh` wrapper, `agent-browser`, `vmm-desktop`. These are the init binary itself under different names, so they work in images without a shell. It is on `PATH` for SSH sessions and for services the init starts; for systemd images via `DefaultEnvironment`.
- The context file (`AGENTS.md`) under that directory; `/etc/claude-code/CLAUDE.md` and `~/.codex/AGENTS.md` handled as today.
- Files the init writes into the image's filesystem, because tools read them from fixed places: the CA trust store, `/etc/environment` block, `/etc/profile.d` snippet, gitconfig include, systemd drop-ins when systemd is present, `/etc/passwd`/`group`/`shadow` for `dev`. All of them are rewritten idempotently on every boot and takeoff, as `vmm-boot.sh` does today.

### User

- Login user **`dev`**, uid/gid 1000, home `/home/dev`, passwordless sudo if the image has `sudo`.
- If the image already has a user with uid 1000 (`ubuntu` in `ubuntu:24.04`, `node` in `node:*`), the init renames it to `dev` (passwd, group, shadow, home directory) instead of adding a second account *(Our call: keeps file ownership inside the image correct)*.
- **No root login over SSH** (decided with the user, for now): hub-managed keys log in only as `dev`, as today with `ubuntu`. Root is reached through `sudo` if the image has it; an image without `sudo` has no root access over SSH. The SSH server rejects `root` and every user other than `dev`.
- Shell: the user's shell from `/etc/passwd` if it exists, else `/bin/sh`, else no shell (SSH then only offers port forwarding, and later SFTP).

### SSH server

- In the init binary (`golang.org/x/crypto/ssh`), listening on port 22 of the VM's addresses. Authorized keys come from the seed (hub-managed) plus `~/.ssh/authorized_keys` of `dev` *(Our call: the user's own keys keep working as with sshd today)*.
- Sessions: shell and exec with PTY, environment, local and remote port forwarding (needed by `observatory connect`, VS Code Remote, the xpra tunnel `xpra attach ssh://dev@…/100`), agent forwarding. Every session process enters the image's PID + mount namespace and runs as the target user with the platform environment.
- Subsystem handling designed so `sftp` is an addition later.
- Must interoperate with OpenSSH clients and VS Code Remote-SSH (which needs exec, port forwarding and a working shell with `tar`/`sh` in the image — the standard image has them).

### Image config (from the OCI config)

- `Env`: applied to SSH sessions and services, below the platform variables (org/VM variables and secrets win on conflict).
- `Entrypoint` + `Cmd`: run as a supervised service when the image has no systemd: as root unless `User` says otherwise *(Our call: `User` is honoured for the service, ignored for login)*, with `WorkingDir`, restarted with backoff when it exits, stdout/stderr to a log file in the image (`/var/log/observatory/cmd.log`, rotated) *(Our call)*.
- `WorkingDir`, `User` (for login), `ExposedPorts`, `Volumes`, `Healthcheck`, `StopSignal`: ignored for login; `StopSignal` is honoured when stopping the service on shutdown *(Our call)*.
- An image with systemd: `CMD` ignored, systemd is the init in the namespace.

### Agent (vsock 1050)

Same protocol and operations as today (`ping`, `takeoff`, `resume`, `trust`), so `internal/node/guestcall.go` keeps its shape. Implemented in Go inside the init:

- `takeoff`: MACs, clock, RNG reseed, hostname, machine-id, network for the renamed/restored NICs, new host keys, grow root, attached disks, CAs, kick an integrations sync, stop a running desktop (as today for forks).
- `resume`: clock, reread the seed (keys, env, CAs).
- `trust`: install CAs into the trust store.
- `ping`: answers once setup is done (no `cloud-init status --wait` anymore).

Everything cloud-init-specific (`adopt_instance`, netplan) disappears.

**Compatibility across node versions** *(Our call)*: hibernated VMs, named snapshots and forks freeze a running init, so after a node upgrade the node may talk to an older init. The protocol stays backward compatible: JSON with additive fields only, unknown fields ignored, a `version` field in `ping` replies so the node knows what the init supports. A restored VM keeps its old init and helpers until its next cold boot; that is acceptable. Templates include the init version in `templateKey` and are rebuilt.

### Integrations (vsock 1051 token service, unchanged node side)

Ported from `vmm-integrations.py` into the init binary: `token` and `credential` as multi-call helpers; `sync` as an internal loop (boot + every 5 minutes + on takeoff) instead of a systemd timer. Same behaviour, same ownership rules (never replace the user's own login/file), same state file (path under `/var/lib/vmm/dev/` or a platform path; builder's call). The Linear MCP sync still calls the image's `claude` if present. The `gh` wrapper execs the image's `gh` if present.

### Egress CA

Same CA content and lifecycle as today. Because images differ, the init updates the system bundle at the paths that exist in the image (`/etc/ssl/certs/ca-certificates.crt` for Debian/Ubuntu/Alpine, `/etc/pki/tls/certs/ca-bundle.crt` for Fedora/RHEL), by appending the platform CAs in a marked block rather than relying on `update-ca-certificates` being installed *(Our call)*, and sets `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE` for sessions, services and systemd as today. If the image has `update-ca-certificates` and runs it later, the block must come back on the next boot/trust.

### Desktop and agent-browser

Work when the image contains xpra (and Chrome for `agent-browser`); otherwise `vmm-desktop` and `agent-browser` say so and exit non-zero. Today they rely on systemd user units with linger for `ubuntu`. *(Our call)*: the init starts and stops xpra itself (on demand, idle stop as today), as `dev`, inside the image namespace, so it works with and without systemd. `vmm-browser.py` moves into the Go binary like the rest.

### Seed

Same seed drive mechanism (`WriteSeedISO`), new content: no `user-data`/`meta-data`/`network-config` for cloud-init and no scripts. One JSON document for the init (hostname, instance id, NICs with MAC and addressing, attached disks, authorized keys, env, egress CAs, context file), shaped by the builder. `ReadSeedFile` users (reseed on resume) adapt.

### Root disk from an OCI image (node)

- Node pulls `ghcr.io/asterism-dev/observatory-guest:<version>` for linux/amd64, verifies digests, stores layers in a content-addressed cache under the images dir.
- Flattens the layers (whiteouts, opaque dirs, hardlinks, xattrs, device nodes, ownership preserved) into an ext4 image without partition table, sized to content plus headroom, cached as `images/<digest>.raw`. Tools: `mkfs.ext4 -d` from an unpacked tree, or a Go ext4 writer — builder's call; it must run without a VM and finish in about a minute for the standard image.
- VM creation clones it (reflink where the filesystem supports it, as `CloneResize` does) and truncates to the VM's disk size.
- Pruning: keep layers and disks referenced by a configured image or by an existing template; others go.
- Which image version a node uses in A: the standard image tag pinned in the node build *(Our call: version-locked like kernel and init; B replaces this with per-VM image choice)*.
- Node image status in the UI (`proto.ImageStatus`: building/ready/failed) stays, with "pulling/converting" instead of "building".

### Standard image

- `deploy/guest.Dockerfile` (path builder's call): `FROM ubuntu:24.04` (pinned by digest), the same tools and versions as `manifest.json` with checksums, apt snapshot, Docker daemon config, Chrome/agent-browser, xpra, `systemd` as init, `sudo`, `openssh-client`, `git`, etc. No sshd needed.
- Built and pushed by the release pipeline (tag = release version + `latest`), public on GHCR.
- `internal/node/guestimage` (manifest, build script), the build VM, `previousGuestImage`/`pruneGuestImages` and the stock-image `imageURL` escape hatch are removed.

### Fast boot, snapshots, hibernate

Template snapshots stay per (image digest, size, NIC count, …): the template boots the new kernel/initramfs/root disk and is snapshotted after the init reports ready; `templateKey` uses the image digest instead of the guest image version. Takeoff after restore goes through the Go agent. Hibernate, named snapshots and forks keep working; forks of a snapshot carry the source's root disk as today.

### Removed

cloud-init and netplan handling (`CloudInit`, `adopt_instance`, `apply_network`), `DiskScript` as shell, kernel extraction from the guest disk (`boot.go`), all Python in `internal/node/guest`, `vmm-boot.sh`, the guest image manifest and build, `stockImage` in the VM API, the `ubuntu` user everywhere (AGENTS.md, CLI `connect` default user, docs).

### Hub and API

- `vms.image` holds the image reference + digest the VM was created from (was the guest image version).
- Cut-over *(Our call)*: hub and nodes are upgraded together (no mixed-version fleet, as today: there is no hub/node version negotiation). The node upgrade does not stop running old-runtime VMs; once stopped or hibernated they cannot be started or resumed on the new runtime. The node keeps no old-runtime code paths.
- Migration: existing VMs are not migrated. *(Our call)*: the migration marks them as needing recreation; the UI/CLI show "created with the old runtime, recreate" and starting them fails with a clear error. Data disks can be detached and attached to a new VM.
- Docs (docs site, AGENTS.md, CLI help) updated for `dev`, the new runtime and image requirements ("none").

## Constraints

- No Python, cloud-init, netplan, systemd or shell required in the image.
- Platform services run outside the image's PID namespace; a user in the image (root included) cannot see or kill them through `/proc` of their namespace. Root in the VM can still do anything to the VM; that is not a security boundary against the VM's owner.
- Secrets and tokens never appear in the console log, as today.
- Boot time of a cold boot must not regress noticeably against today; fast boot stays in the sub-second range.
- Tests stay hermetic (no real credentials, fake `PATH`, temp `HOME`), and KVM tests run on the Proxmox test nodes.

## Left to the builder

- Kernel config details and build caching in CI; delivery of kernel + initramfs to nodes within the version-lock constraint.
- Exact platform directory name and layout, state file paths.
- ext4 creation tooling, layer cache layout, pull client library.
- How OVN NIC addresses reach the init (DHCP client in the init or addresses in the seed).
- Supervisor backoff and log rotation details.
- Whether `resize2fs` runs online or offline.
