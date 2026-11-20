# Images

An image is an org resource: the starting point of a VM's root disk. Besides the platform's standard image
([guest.md](guest.md)) you can keep your own, either as a **recipe** (a Dockerfile plus files) that observatory builds in
a throwaway builder VM, or as an image you built locally and **pushed**. Versions land in the hub's registry, nodes pull
only what they lack, and new VMs pick an image explicitly or through a **default**.

```
observatory image create my-tools
observatory image recipe push my-tools ./my-tools
observatory image build my-tools --follow
observatory image default set user my-tools
observatory vm create dev --wait
```

Only `linux/amd64` images are supported. An image index is accepted when it contains a `linux/amd64` manifest;
the version keeps the index digest, and nodes pull and cache only that manifest.

## Scopes and visibility

| Scope | Who sees and uses it | Who edits and builds it |
|---|---|---|
| `private` (yours) | only you | only you |
| `team` | the team's members | team maintainers and org admins |
| `org` | every member of the org | org admins |

The standard image is the read-only platform image `observatory/standard`, listed in every org.

Visibility can be changed on the image page (`observatory image visibility <ref> --scope private|team|org [--team T]`).
**Widening** it (private to team or org, team to org) **deletes the existing versions**, because they were built with
access to the narrower scope's secrets and must not be shared. The web UI and the CLI ask first; the API answers `409`
with the error `code` `widen_deletes_versions` until the request carries `confirmDeleteVersions: true`. The next build or
push creates shared versions; a push-only image is shared by pushing again. **Narrowing** keeps the versions, and VMs
created earlier keep running either way. Images without a recipe cannot be built, so they need a new push.

## References

Everywhere an image is named (`vm create --image`, defaults, the CLI, the create dialog) you can use:

| Form | Meaning |
|---|---|
| `name` | the image `name`, tag `latest` |
| `name:tag` | a tag |
| `name@sha256:<64 hex>` | an exact version; a VM records this digest |
| `me/name`, `<team>/name`, `org/name` | qualified by scope: your private image, a team's, the org's |
| `observatory/standard` | the platform image |

`me`, `org` and `observatory` are reserved and cannot be team names.

A bare name is looked up in your private images, then the VM owner's team, then the org. If it matches in more than one
place the request fails and lists the qualified forms to choose from. A VM stores the reference and the digest it was
created from; deleting versions or images never touches existing VMs.

## Default image

A new VM without an image takes the default of its owner, falling back to `observatory/standard`:

- a **personal VM**: your default, else the org default;
- a **team VM**: the team's default, else the org default.

Team defaults never apply to personal VMs and personal defaults never to team VMs. Set defaults in the org, team and user
settings of the web UI or with `observatory image default set <org|user|team:T> <ref>`; the create dialog shows the
preselected image and where it comes from. You need the right to use the image and to change settings at that level.

## Recipes

A recipe is a set of files with a `Dockerfile` at the root. Every edit creates a new immutable revision, and a build uses
the image's current revision. Edit text files in the web UI and upload others, or work in a directory:

```
observatory image recipe pull my-tools ./my-tools       # prints the --ref flags of the revision
observatory image recipe push my-tools ./my-tools --ref integration:private/github
```

Limits: 50 MB in total and 1000 files per revision; paths are relative and clean; `Dockerfile` is required at the root.

A recipe may list **references** to secrets and integrations it uses (`--ref secret:<scope>/<NAME>` or
`--ref integration:<scope>/<kind>`, scope `org`, `private` or `team:<team>`). The scope rule: only references at the
image's level or broader are allowed (private image: private, team and org; team image: team and org; org image: org
only). A private recipe that references private secrets cannot be shared; see widening above. The person who starts a
build needs the right to use every reference, checked at build time, so a recipe edited by someone else cannot borrow
your access.

## Builds

`observatory image build <ref> [--follow]` (or the Build button) queues a build of the current revision. The hub starts
a hidden **builder VM** on a node of the org (2 vCPU, 4 GB RAM, 40 GB disk). While it runs it counts against the vCPU,
RAM, VM count and disk limits of the org and of its owner (the image's team or user; for an org image, whoever started
the build); when one would be exceeded, the build fails naming that limit. The builder runs BuildKit for `linux/amd64`,
pushes the result and is deleted afterwards, whether the build succeeded or not. No build code runs outside a VM.

- A build times out after 60 minutes and then fails.
- One build per image at a time; another request is rejected while one runs.
- Cancel a queued or running build on the image page or with `observatory image cancel <build-id>`.
- A successful build adds a version and moves the tag `latest` to it. A failed or cancelled build never moves `latest`,
  but when its push landed before it failed or was cancelled, that version and its `build-*` tag remain.
- Builder VMs are not in the VM list. Org admins see running ones under the org's builder VMs in the web UI, or with
  `observatory image builders`.

### Logs

Every build keeps its log, succeeded, failed or cancelled, until the image is deleted; deleting a version keeps its build
and log. Follow the running build live in the web UI or with `observatory image build <ref> --follow`; list builds with
`observatory image builds <ref>` and print any log with `observatory image log <build-id> [--follow]`. The web UI also
downloads a log as text. Values of the referenced secrets and integrations are masked in logs. A log above 10 MB keeps
its start and end and cuts the middle with a marker.

### Build secrets

References reach the build as BuildKit secrets with a stable id: the variable name for a secret, the kind for an
integration (`github`, `linear`, `claude`). Nothing is added to `RUN` steps implicitly: use
`RUN --mount=type=secret,id=<id>`. The value exists only for that step and never lands in a layer. The same Dockerfile
works locally with `docker build --secret id=<id>,src=<file>`.
A recipe cannot reference `gcloud`: its real Google token never leaves the hub, so builds cannot use it.

Clone a private repository with the GitHub integration (reference `integration:private/github`):

```dockerfile
RUN --mount=type=secret,id=github \
    git -c http.extraHeader="Authorization: Basic $(printf 'x-access-token:%s' "$(cat /run/secrets/github)" | base64 -w0)" \
    clone https://github.com/org/repo /opt/repo
```

The header form keeps the token out of `.git/config`; a token in the clone URL would be stored there, in the layer.

Install private npm packages with a secret `NPM_TOKEN` (reference `secret:private/NPM_TOKEN`):

```dockerfile
RUN --mount=type=secret,id=NPM_TOKEN \
    echo "//registry.npmjs.org/:_authToken=$(cat /run/secrets/NPM_TOKEN)" > /tmp/npmrc \
    && NPM_CONFIG_USERCONFIG=/tmp/npmrc npm ci && rm -f /tmp/npmrc
```

Install from a private pip index with a secret `PIP_INDEX_URL` (the URL contains the credentials):

```dockerfile
RUN --mount=type=secret,id=PIP_INDEX_URL \
    PIP_INDEX_URL=$(cat /run/secrets/PIP_INDEX_URL) pip install -r requirements.txt
```

Hosts the build talks to must be allowed by the egress proxy like for any VM ([egress.md](egress.md)).

### Base images

`FROM` can name the standard image (`ghcr.io/asterism-dev/observatory-guest:<version>`), public images, or another
observatory image by its registry path `FROM <hub host>/i/<image-id>[:tag]` (the id is on the image page; `<hub host>` is
the host of the hub URL, as in `observatory login`). A build may pull the observatory images that pass the scope rule
(at the building image's level or broader) and that the person who started the build may use. Anything else is denied by
the registry.

The hub copies its tags into the registry whenever one changes (a build moves `latest`, a push, `image tag`), so a pull by
tag gets the version the image page shows. A tag deleted on the hub stays in the registry until the next push of that
tag; pull by digest when that matters.

### Automatic rebuilds

An image with a recipe can rebuild itself so updates of its base images arrive without anyone remembering. Both are off
by default; turn them on in the image settings (Automatic rebuilds) or with
`observatory image auto <ref> [--schedule off|daily|weekly] [--on-base-change=true|false] [--approve]`:

- **Schedule:** daily or weekly, counted from the last successful build, manual or automatic.
- **When a base image changes:** every build records the `linux/amd64` digest each `FROM` resolved to (`ARG` defaults
  declared before the first `FROM` are expanded; stage names, `scratch` and `FROM <image>@<digest>` are left out). The
  hub resolves those references again at most once an hour (observatory images from its own tags, public images
  anonymously, only on public addresses) and rebuilds when one differs from what `latest` was built from. A pushed
  `latest` has no recorded bases, and neither has a version built before automatic rebuilds existed: such an image
  reacts to base changes only after its next build.

The hub looks every 10 minutes. It never starts an automatic build while a build of the image is queued or running, and
at most one automatic build every 6 hours per image. Automatic builds use the normal queue and limits, are marked
automatic in the build history and run as the person who last changed the settings: changing them needs the right to
edit and to build the image, and that person's right to build and to use every reference is checked at each automatic
build. When it is missing, the build fails with the reason and automation stays on.

Turning automation on approves the recipe revision shown at that moment. A later revision saved by someone else is only
built automatically when its author may build the image and use every reference too; otherwise automatic builds fail
until someone with the right to edit and build reviews it and approves it: the button "Approve revision N for automatic
builds" in the image settings, or `observatory image auto <ref> --approve`. An approval names the revision that was
shown and is refused when a newer one was saved meanwhile; whoever approves becomes the person automatic builds run
as. Other changes of the settings approve nothing. After an approval the next automatic build may start at once, without
waiting out the 6 hours since the last one.

`latest` only moves when a build succeeds. When an automatic build fails, or the base images cannot be looked up (for
example the registry is down; nothing is built then and the next pass tries again), the image shows "automatic rebuild
failed" with the reason in the web UI and in `observatory image show`; the next successful automatic build or a change
of the settings clears it. A failed build's reason is not replaced by a later failed lookup. With a schedule, a recipe
that keeps failing is retried every 6 hours, because the schedule counts from the last successful build.
A rebuilt base image triggers the images built on it through their own base check, one level per pass. Rebuilt images
are not propagated to existing VMs, only VMs created afterwards use them.

## Pushing

Build locally with `docker build` and push:

```
observatory image login                                  # once: docker authenticates through the CLI session
docker push <hub host>/i/<image-id>:<tag>
observatory image push my-tools:v1 my-tools:local        # tags and pushes with the local docker
```

`image login` sets `credHelpers` for the hub host in `~/.docker/config.json` (other keys stay) to
`docker-credential-observatory`, which exchanges your CLI session for short-lived registry tokens; log in with
`observatory login` first. `image push` without a tag pushes `latest`. A push needs the right to build the image. Every
tagged push becomes a version, sets its tag and moves `latest`. A push by digest alone becomes no version.

A push the hub cannot turn into a version (not `linux/amd64`, over the storage limit, an
unreadable manifest or config) is removed again and the reason
is shown on the image.

## Storage limit

The org limit **image storage (GB)** counts the sizes of all versions of the org's images, shared layers once. It is
set by a platform admin in the web UI (`observatory limit show` shows it as `images`). An org that is over the limit
gets no push tokens: builds and pushes are refused up front. A push that crosses the limit is accepted by the registry
but not turned into a version, and the manifest is deleted. Deleting versions or images frees the quota at once and the
disk after garbage collection ([deploy.md](deploy.md#image-registry)).

## Nodes

Nodes pull with their own credential and keep several images in a cache, evicting the least recently used when space is
needed, never an image a template, a VM being prepared or a pull in flight uses. The first VM of an image on a node waits
in `starting` while the image is pulled; the next one starts without pulling.

## Not supported

Vulnerability scans, images shared across orgs, public images and arm64.
