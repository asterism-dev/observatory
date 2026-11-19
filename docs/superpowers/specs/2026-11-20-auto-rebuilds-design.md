# Automatic image rebuilds

## Intent

Follow-up to `2026-11-19-images-design.md`. Builds run only on demand today, so security updates in a base image (`FROM ubuntu:24.04`, `FROM <hub>/i/<id>`) reach an image only when someone remembers to rebuild. The user decided during the images brainstorm: builds by hand first, automatic rebuilds as a separate small project — per image, on a schedule and/or when the base image changes; `latest` moves only on success; a failed automatic build is visible on the image.

Success: an org admin enables "weekly + when the base changes" on `org/basis`. When `ubuntu:24.04` gets a new digest, a build starts within the hour, succeeds, and `latest` moves; VMs created afterwards get it. When a referenced secret was deleted, the automatic build fails, `latest` stays, and the image shows "automatic rebuild failed: …" in web and CLI.

## Decided with the user

- Per-image settings, off by default: schedule (off | daily | weekly) and "rebuild when a base image changes".
- `latest` only moves on a successful build (same as manual builds).
- Failures are visible on the image (web + CLI); the previous version stays active.

Everything else is *(Our call.)*

## Goals / non-goals

- Goals: settings API/CLI/web; hub-side trigger loop; base-image change detection for public registries and observatory images; visible last automatic result.
- Non-goals: notifications (mail/chat), rebuilding pushed-only images (no recipe), cron expressions, per-tag schedules, propagating rebuilds transitively in one go (a rebuilt base triggers its dependents through the normal base-change check on the next pass).

## Approaches

- **Hub loop + stored base digests (chosen):** every 10 minutes the hub checks images with automation on: schedule due, or base digests changed since the last successful build. Simple, no new components, reuses the build queue.
- Registry webhooks from upstream registries: not available for public registries — rejected.
- Node-side checks: nodes don't own images — rejected.

## Design *(Our calls)*

- **Settings** on `images`: `auto_schedule text` ('', 'daily', 'weekly'), `auto_on_base_change bool`, `auto_by uuid` (the user who last changed the settings; automatic builds run with that user as starter — their `image:build` and `use` rights on every reference are checked at each automatic build; if missing, the build fails with a clear error and automation stays on), `auto_error text`, `auto_last_at timestamptz`, `auto_revision int` and `auto_approved_at timestamptz` (the newest recipe revision approved for automatic builds, and when). Migration `00025_auto_rebuilds.sql`. Changing settings requires `image:edit` + `image:build`.
- **Approval** (added after review): automatic builds also check the author of the revision being built (same rights as `auto_by`) unless the revision is by `auto_by` or at most `auto_revision`. Approval is explicit and pinned: the client sends the revision it displayed (`approveRevision`), the server stores it only if it is still current (else 409 `recipe_changed`) and the approver becomes `auto_by`. Turning automation on sends the shown revision; other settings changes approve nothing. Builds before the approval do not count against the 6 h limit.
- **Base digests**: when a build succeeds, the build agent reports the resolved digest of every `FROM` it pulled (BuildKit metadata `containerimage.config.digest` is not enough — the agent resolves each FROM reference itself before building: parse the Dockerfile's `FROM` lines (ignoring stage names and `--platform`, expanding `ARG` defaults declared before the first FROM), resolve each external ref to a linux/amd64 manifest digest anonymously (public registries) or with the build's registry credential (`<hub>/i/<id>`), pass the pinned list in the result). The hub stores them on the version (`image_versions.base_digests jsonb`: `[{"ref": "...", "digest": "..."}]`).
- **Trigger loop** (hub, every 10 min, one org at a time):
  - Schedule due: daily = last automatic or manual successful build older than 24 h; weekly = 7 days.
  - Base change (if enabled): re-resolve each `ref` of `latest`'s `base_digests` (same resolution rules; hub-local lookup for `<hub>/i/<id>` via the DB tags) — any difference triggers. At most one base-change check per image per hour, at most one automatic build per image per 6 h, never while a build is queued/running.
  - Starts a normal build (same queue, same limits) with `started_by = auto_by` and a flag `automatic = true` on `image_builds` (shown in history).
  - Result: success clears `auto_error`; failure sets `auto_error` to the build's error; resolution errors (registry down) set `auto_error` without building and retry next pass.
- **API**: `PATCH /images/{id}` accepts `autoSchedule`, `autoOnBaseChange`, `approveRevision`; image JSON gains `autoSchedule`, `autoOnBaseChange`, `autoError`, `autoLastAt`, `autoRevision`, `autoNeedsApproval`; build JSON gains `automatic`.
- **CLI**: `observatory image auto <ref> [--schedule off|daily|weekly] [--on-base-change=true|false] [--approve]`; `image show` prints automation and the last automatic error; `image builds` marks automatic builds.
- **Web**: image settings tab gets an "Automatic rebuilds" section; the image header shows a warning tag when `autoError` is set; build history marks automatic builds (de + en).
- **Docs**: images.md section "Automatic rebuilds".

## Constraints

- No new secrets exposure: automatic builds use exactly the same reference rules as manual builds, checked against `auto_by`.
- Registry/network failures during checks never start builds and never flap `latest`.

## Left to the builder

Dockerfile FROM parsing details (BuildKit's parser package may be used), resolution caching, loop timing within the stated bounds.
