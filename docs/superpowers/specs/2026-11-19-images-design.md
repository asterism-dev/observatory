# Images (registry, builder, versions, defaults)

## Intent

Project B of "own images". Project A (`2026-11-19-guest-runtime-design.md`, PR #24) made any linux/amd64 OCI image bootable as a VM, but every node still uses one image: the standard image, or `firecracker.guestImage` from node.json. Users have different tooling, want lightweight images, and want to build them the way they know: a Dockerfile, tested locally with `docker run`. This project makes images a resource of the org: users store a build recipe (Dockerfile plus files) in observatory and build it in a throwaway VM, or push an image they built locally; versions land in an observatory registry; nodes pull only what they lack; a new VM picks an image, with a default per org, team and user.

Success: a member creates the private image `my-tools` in the web UI, writes a Dockerfile `FROM ghcr.io/asterism-dev/observatory-guest:<version>` that installs `ripgrep` and clones a private repo with the GitHub integration, clicks "Build", watches the log, and the image gets version `sha256:…` with tag `latest`. They set it as their personal default, create a VM, and `rg --version` works; the repo is there. A colleague pushes `docker push <hub>/…` an image built on their laptop into a team image and creates a VM from `team-image@sha256:…`. An org admin sets `basis:latest` as the org default; members without their own default get it. A second VM from the same image on the same node starts without pulling.

## Decided with the user

- **Images are an org resource with visibility org, team or private**, like integrations.
- **The recipe (Dockerfile + files) is stored in observatory and can be rebuilt.** Each build is a new version from the recipe's current revision.
- **Two ways in:** build from the stored recipe in a throwaway builder VM on a node; or push a locally built image from the CLI (`docker push` / `observatory image push`).
- **Builds can use integrations and secrets, by explicit reference in the recipe.** A recipe references concrete secrets/integrations; only scopes at the image's level or broader: private image → private, team and org; team image → team and org; org image → org only. Only someone allowed to use those references may build.
- **Private images are built only by their owner.** Sharing a private image with a team is allowed only when its recipe references no private secrets; existing versions are not shared, sharing starts with a new build.
  - Implementation *(Our call)*: widening visibility (private → team/org, team → org) deletes the existing versions after a warning in the UI; the next build or push creates shared versions (a push-only image is shared the same way, by pushing again). Narrowing visibility keeps versions; other users' existing VMs keep running.
- **Default image for new VMs: cascade org → team → user**, the most specific wins; the create dialog shows the preselected image and where it comes from.
- **Builds only on demand** (button / CLI). Automatic rebuilds (schedule, base image changed) are a later project.
- **`latest` moves on every successful build or push** of that image, for all images. Any version can be used by digest (`name@sha256:…`) to test it.
- **Versions are immutable digests; tags are movable.** A VM records the digest it was created from; deleting versions or images never touches existing VMs.
- **Registry: a `distribution` container in the compose stack, the hub issues tokens** (Docker token auth). Approach a below.
- **Recipes stored in Postgres.**
- **Default cascade by VM owner:** a personal VM takes the user's default, else the org default; a team VM takes the team's default, else the org default. Team defaults never apply to personal VMs, personal defaults never to team VMs.
- **Build-time secrets are explicit in the Dockerfile:** referenced secrets and integrations are BuildKit secrets, used with `RUN --mount=type=secret,id=…`; the real value exists only for that step and never lands in a layer. The docs ship snippets (git clone with the GitHub integration, npm, pip). Same Dockerfile works locally with `docker build --secret`.
- **Every build keeps its log**, succeeded, failed or cancelled.

Everything else is marked *(Our call.)*

## Goals

- Image resource: CRUD, visibility, permissions, recipe editing (web + CLI), builds with live logs, pushes, versions and tags, deletion, defaults cascade.
- Registry in the deploy stack with hub-issued scoped tokens for CLI pushes, builder pushes and node pulls.
- Builder VMs from a platform `observatory-builder` image, with the recipe's referenced secrets and integrations.
- VM creation by image reference (`name`, `name:tag`, `name@digest`); nodes pull from the registry with node credentials and keep a cache of several images.
- Standard image visible to every org as a read-only platform image.
- Limits: registry storage per org; builder VMs count against org CPU/RAM; build timeout.

## Non-goals

- Automatic rebuilds, base-image tracking, vulnerability scans.
- Sharing images across orgs; public images.
- arm64 images.
- Pulling user images from external registries directly by reference (users `FROM` them in a recipe, or push them).
- A full code editor; the web UI edits text files and uploads others.
- Changing the guest runtime (project A).

## Approaches

Registry:
- **a) `distribution` (registry v3) container behind Caddy at `/v2/`, hub as token server** *(chosen)*: proven storage, garbage collection, resumable uploads, compatible with every docker/OCI client; we only write authorization. Cost: one more container; usage per org comes from registry notifications + hub metadata.
- b) Registry embedded in the hub binary: no new container, but either a heavy library or our own `/v2` implementation (uploads, chunking, GC) to maintain.
- c) Extend the gateway's storage service: it exports block devices over NBD, not HTTP objects; does not fit.

Recipe storage:
- **Postgres, one row set per recipe revision, capped** *(chosen)*: simple, versioned, covered by existing backups.
- OCI artifact per revision in the registry: dedup and no size cap, but awkward to edit in the UI.

Builder: a hidden, regular VM from a platform image is the one sensible way — build code from users must not run on the hub or a node host, and a VM gets integrations, secrets, egress proxy and limits the same way as any VM.

## Design

### Model *(Our call, shapes left to the builder within this)*

- `images`: id, org, scope (org | team | user) + owner id, name (unique per scope owner, same name rules as VMs), description, created_by, created_at, current recipe revision (nullable — pushed-only images have none), `platform` flag for the standard image (one global row, not per org).
- `image_recipe_revisions`: id, image, number (1, 2, …), files (path → content, mode), references (list of secret/integration references with their scope), created_by, created_at. Revisions are immutable; editing creates a new one. Cap: 50 MB total, 1000 files, paths relative and clean, `Dockerfile` required at the root *(Our call)*.
- `image_versions`: image, digest, source (build | push), recipe revision (for builds), build id (for builds), size, created_by, created_at.
- `image_tags`: image, tag, digest. `latest` set by every successful build/push; users may set other tags.
- `image_builds`: id, image, revision, state (queued, running, succeeded, failed, cancelled), node, started/finished, error, log. Every build keeps its log (decided with the user) until the image itself is deleted; deleting a version keeps its build and log. Logs stored compressed; above 10 MB the middle is cut with a marker, keeping the start and the end *(Our call)*.
- Default image settings: org / team / user level, each holding an image reference (image id + tag or digest); resolution by VM owner as decided above.

### Permissions *(Our call)*

New rights per scope, mirroring integrations' object layout (org object, team object per team, user object per user): `image:list`, `image:use` (create VMs from it), `image:edit` (recipe, tags, delete versions/images, visibility), `image:build` (build and push). Built-in roles: org admins have all rights on org images, team maintainers on their team's images, members `list`+`use` on org and their teams' images; private images reach only their owner. Building additionally requires `use` on every referenced secret/integration (existing env/integration use rights). Setting a default requires `use` on the image and the right to change settings at that level (org admin, team maintainer, the user).

### Registry and auth

- `distribution` container in `deploy/compose.yaml` (and the dev compose), storage on a volume (S3 optional via its own config), not exposed itself. *(Our call)*: the hub reverse-proxies `/v2/` to it, so every deployment (with or without Caddy, dev included) serves the registry under the hub URL; clients need the hub over https as docker requires (nodes and builder VMs may use the plain internal URL). Registry configured with `auth: token` pointing at the hub's token endpoint and the hub's signing key *(Our call)*.
- Repository path per image: `<host>/i/<image-id>` *(Our call: stable across renames and visibility changes; the CLI and UI show it, users rarely type it)*.
- Hub token endpoint (Docker token spec): grants `push,pull` on `i/<id>` to CLI sessions with `image:build`, `pull` with `image:use`; to builder VMs for exactly their image (one-shot credential from the hub, valid for the build); to nodes `pull` on images of their org. Tokens are short-lived (5 min). `docker login <hub>` uses a CLI-issued credential: `observatory image login` writes a docker credential helper config (`docker-credential-observatory`) that exchanges the CLI session *(Our call)*.
- Registry notifications → hub webhook: record pushed manifests (digest, size), so pushes create versions and move `latest`, and usage per org is known. A push of a manifest to `i/<id>` whose config is not linux/amd64 is accepted by the registry but rejected as version with a visible error *(Our call)*.
- Garbage collection: deleting versions deletes manifests via the registry API; a periodic `registry garbage-collect` in the container (maintenance window, read-only mode during GC) reclaims blobs *(Our call; the builder chooses the schedule)*.

### Builds

- "Build" queues a build for the image's current revision. The hub picks a node in the image's org with free capacity (same placement as VMs) and creates a hidden builder VM: image `observatory-builder:<version>` (platform image built by the release pipeline: BuildKit + a small build agent), 2 vCPU / 4 GB / 40 GB by default *(Our call)*, counted against org limits while it runs, integrations and env resolved **only from the recipe's references**, egress proxy as for any VM.
- The build agent fetches the revision from the hub (authenticated per build), runs BuildKit (`linux/amd64`, every reference exposed as a BuildKit secret with a stable id: integration kind for integrations, e.g. `github`, and the variable name for secrets; nothing is injected into `RUN` steps implicitly), pushes to `i/<id>` with the per-build token, streams logs to the hub, reports the digest, and the VM is deleted. Timeout 60 min, then the build fails and the VM is deleted *(Our call)*.
- Logs: live in the UI and `observatory image build --follow`, then kept with the build (see model); `observatory image builds <name>` lists builds and `observatory image log <build>` prints any build's log. Secret values never appear in logs (the agent masks referenced values) *(Our call)*.
- One build per image at a time; a new request while one runs is rejected *(Our call)*.

### Pushes

`observatory image push <name>[:tag] <local-image>` tags and pushes via the local docker (needs `docker`), or users run `docker push <host>/i/<id>:<tag>` after `observatory image login`. A push sets the given tag and `latest`.

### Platform images

The standard image appears in every org's image list as read-only `observatory/standard`, source `ghcr.io/asterism-dev/observatory-guest`. It is the fallback when no default is set. *(Our call)*: the node keeps choosing its version as today (matching its own version, or `firecracker.guestImage`) and reports the digest; the hub stores only `observatory/standard` until the node reports the digest. Pinning the standard image to a digest is not offered.

### VMs and nodes

- Image references *(Our call)*: `name`, `name:tag`, `name@sha256:…`, optionally qualified as `me/name`, `<team>/name`, `org/name`; the platform image is `observatory/standard`. A bare name is looked up in the caller's private images, then the VM owner's team, then the org; if it matches more than one, the request fails listing the qualified forms.
- VM create (web, CLI, API) takes an image reference or nothing (cascade). The hub resolves it to a digest at creation and stores `image ref + digest` on the VM; the node gets the digest and the repository.
- `firecracker.guestImage` stays as the node-level override for the platform fallback only.
- Nodes pull from the registry with a node credential (hub token endpoint, `pull` on their org's images) via the existing `ociimage` store; the registry URL comes from the hub. The cache keeps several images: prune by least recently used and free space, never an image referenced by a template, a VM being prepared, or a pull in flight *(Our call)*.
- Templates are per image digest (already the case after A).

### Limits

New org limit "image storage GB" (registry usage counted per org from version sizes, shared layers counted once per org). Enforcement *(Our call)*: an org already over its limit gets no push tokens (builds and pushes refused up front); a push that crosses the limit is accepted by the registry but not turned into a version — the hub deletes the manifest and shows the error, like a non-amd64 push. Builder VMs count against CPU/RAM limits of the image's scope while running.

### UI and CLI

- Web: Images page (list per scope with tags, size, last build), image detail (recipe files with editor for text and upload for others, references picker, builds with live log, versions and tags, delete, visibility), default image selectors in org, team and user settings, image picker in "Create VM".
- CLI: `observatory image list|show|create|delete`, `image recipe pull|push <dir>`, `image build [--follow]`, `image push`, `image login`, `image tag`, `image versions`, `image default set|show`, `vm create --image`.
- Docs: images page, recipe reference, `docker push` how-to, limits.

## Constraints

- Builder VMs are hidden from the VM list but visible to org admins in an admin view, and are deleted after every build, success or not.
- No user build code runs outside a VM.
- Secrets and tokens never in logs, recipes or image metadata.
- Every API change is mirrored in CLI and web, and documented (de + en UI).
- Tests hermetic; registry tests run against a real `distribution` container in CI where possible.

## Left to the builder

- Exact table shapes, migration layout, the build agent's protocol with the hub.
- Registry config details, GC schedule, notification handling.
- BuildKit invocation and caching inside the builder VM.
- Web layout within the existing UI design system.
