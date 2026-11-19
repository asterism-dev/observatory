# SFTP in the guest SSH server

## Intent
The user wanted SFTP in the built-in SSH server of the guest init (decided in the guest-runtime design, deferred until now). File transfer with `sftp`, `scp` (OpenSSH ≥ 9 uses SFTP), editors' remote file browsers and IDE remotes must work as `dev` in every image, including images without a shell or an `sftp-server` binary.

## Decided with the user
- SFTP is served by the platform init, like SSH itself; nothing required inside the image.
- Login stays `dev` only; no root.

## Design (our calls)
- The `sftp` subsystem in `internal/guestinit` runs a helper `observatory-init sftp-server` inside the image's PID namespace and root as uid/gid 1000 with dev's groups, `HOME=/home/dev`, cwd `/home/dev`, exactly like a session process (same spawn path, same cgroup). The helper serves SFTP on stdin/stdout with `github.com/pkg/sftp` (server mode). Running as dev means the kernel enforces permissions; no path filtering in the server.
- The channel is wired to the helper's stdin/stdout; stderr goes to the init log; exit status sent on the channel.
- `scp -s`/legacy scp: legacy scp needs a shell and `scp` in the image — unchanged; modern scp uses SFTP and works.
- Docs: guest.md / cli.md mention SFTP (`sftp dev@<vm>.internal`, VS Code / Zed remote file access).

## Approaches
- Helper process inside the image as dev (chosen): permissions enforced by the kernel, same isolation as a shell session.
- In-process SFTP in PID 1 with path checks: runs as root outside the image, needs its own path confinement and permission emulation — rejected.

## Tests
- Unit: subsystem `sftp` dispatches to the spawn with the helper argv and dev credentials (fake spawn).
- Linux: an OpenSSH `sftp` client against the server (existing OpenSSH test harness) can put/get/list/remove a file in a temp root; a file owned by root and mode 0600 cannot be read.
