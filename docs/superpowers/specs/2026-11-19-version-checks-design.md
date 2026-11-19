# observatory: version checks and CLI self-update

## Intent

Admins and users should stay current. Today nothing tells anyone that a newer release exists: the hub runs a pinned
`OBSERVATORY_VERSION`, the CLI is copied by hand, and nodes keep the version they were installed with until someone
re-runs the installer.

This is project 1 of 2. Project 2 (updating nodes from the hub) gets its own design; see "Follow-up" below.

## Goals

- The hub checks for the latest GitHub release and platform admins see a banner in the web UI when one is newer.
- The web UI shows when a node runs an older version than the hub.
- The CLI warns when a newer release exists and `observatory update` replaces itself with it.

## Non-goals

- The hub updating itself: it runs as a Docker image, the banner only tells admins to bump `OBSERVATORY_VERSION`.
- Updating nodes from the hub (project 2).
- Package-manager installs (Homebrew etc.).

## Anti-goals

- Update checks must never break or slow down normal use: GitHub unreachable, rate-limited or air-gapped means no hint,
  never an error.
- CLI hints must not pollute stdout (`--json`, pipes).

## Approaches

- **A: everything follows the latest GitHub release (picked).** Hub and CLI each ask
  `api.github.com/repos/asterism-dev/observatory/releases/latest`; the CLI updates to the latest release. Simple and
  matches "always current". A CLI newer than its hub is accepted; the hub banner keeps admins moving.
- **B: the CLI follows its hub's version.** CLI and hub always match, but the CLI is only as current as the hub and
  has no target without a login. Rejected.

## Design

**Hub**
- A background loop checks `releases/latest` every 6 hours *(our call)* and keeps the tag in memory. Failures are logged
  only; `latestVersion` stays at its last known value or empty.
- The response that already carries `user.platformAdmin` (`internal/api/api.go:154`) gains `version` (the running hub)
  and `latestVersion`.

**Web**
- Hub banner in `AppShell`, platform admins only, when `latestVersion` > `version` (semver): "Version X available
  (running Y)", linking the GitHub release notes. Dismissable per version, remembered in localStorage *(our call)*.
- Node outdated hint: node list and node page show "runs vX, hub vY" when a node reports an older version than the hub.
  Visible to whoever may see the node page. Hint only; the update action comes in project 2.

**CLI**
- At most once a day *(our call)* the CLI checks `releases/latest`, caching the result and the check time in its
  existing `configDir`. When newer, it prints a one-line hint to stderr naming `observatory update`.
- `observatory update` downloads `observatory_<v>_<os>_<arch>.tar.gz` and `SHA256SUMS` from the release, verifies the
  checksum, and atomically replaces the running binary (write next to it, rename over it).

**Both**
- Builds whose version is not semver (dev builds) never check.
- `OBSERVATORY_NO_UPDATE_CHECK=1` disables the check in hub and CLI (air-gapped, CI) *(our call)*.

## Left to the builder

- Exact HTTP timeouts, the cache file name and format, banner wording and styling (use the existing `ui/` components).
- Where the version comparison lives (one small shared helper is enough).

## Follow-up: project 2, node updates from the hub

Decided so far, to be designed separately:
- `install.sh` gains an update mode that reuses the node's credential (no token, no offline phase); the node runs it via
  `systemd-run` on a hub "update" message, so one path keeps doing host setup.
- Buttons on the node pages: update one node, or "update all" as a rolling update, one node at a time, the next only
  once the previous is back online on the new version; a failure stops the run.
