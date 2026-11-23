# Image recipes from a GitHub repository

## Intent

A user keeps the Dockerfile and build context of an image in a GitHub repository (for example a personal image with
their tools and config) and wants observatory to build from it, instead of copying files into a recipe with
`observatory image recipe push` after every change. The repository is the source of truth; the image follows it on
each build.

## Goals

- An image with a recipe can name a GitHub repository as its source: `owner/repo`, a ref (branch or tag; default: the
  repository's default branch) and an optional subdirectory that holds the `Dockerfile`.
- Every build, manual or scheduled, builds the newest commit of that ref.
- The build history and versions show which commit was built.
- Private repositories work through the GitHub integration; public ones also without.

## Non-goals

- No build per commit: no webhooks, no GitHub App, no polling for new commits. The existing schedule (daily/weekly)
  and manual builds are the only triggers (user decision).
- No other forges (GitLab, Gitea), no GitHub Enterprise Server (the GitHub integration has none either).
- No submodules, no Git LFS content (the tarball API does not include them).

## Anti-goals

- The GitHub token must not reach the builder VM through this feature (the fetch happens on the hub). A recipe that
  also needs the token inside `RUN` keeps using `--ref integration:<scope>/github` as today.
- A source change must never silently run with references (secrets, integrations) the person who started the build
  may not use: the existing checks at build time stay.

## Decisions (user)

- Approach A: the hub downloads the commit as a tarball and stores it as a normal recipe revision; everything after
  that (builder VM, logs, limits, versions, base-image check) is unchanged. Rejected: B, the builder clones (BuildKit
  git context) — loses the revision model, needs the token in the builder, duplicates automatic-rebuild logic.
- Triggers: only manual builds and the existing schedule. No new-commit trigger.
- A repository is trusted like the person who set the source: commits are built without the per-revision approval
  that automatic builds require for revisions saved by someone else, also for team and org images. Documented
  plainly.

## Behaviour

### Setting the source

- `PUT /api/orgs/{org}/images/{id}/source` `{"github": {"repo": "owner/repo", "ref": "main", "path": "image"}}`;
  `ref` and `path` optional; `DELETE …/source` removes it. Needs `image:edit` and `image:build` on the image, like the
  automatic-rebuild settings. Audited (`image:edit`, action `source`).
- CLI: `observatory image source <ref> github owner/repo[@ref][:path]` and `observatory image source <ref> --none`;
  `observatory image show` prints the source and the last fetched commit.
- Web UI: on the image page, the recipe section gets "Source: Files | GitHub repository" with the three fields. With
  GitHub set, the file editor is read-only and shows the last fetched revision.
- Setting a source on an image without a recipe is allowed; the first build creates revision 1.
- Validation: `repo` matches GitHub's owner/name rules; `path` is relative and clean (same rule as recipe paths);
  `ref` is a branch or tag name (no SHA pinning; pin by tag).
- Setting, changing or removing the source behaves like saving the automatic-rebuild settings (`SetImageAuto`): the
  person becomes `AutoBy` (automatic builds run as them), `auto_error` is cleared and an approval by someone else is
  dropped. Otherwise someone with only `image:edit` could point the source at their own repository and have it built
  with another person's references. (Builder-check ruling, follows the trust decision.)
- A fetched revision is authored (`CreatedBy`) by the person the build runs as: who started a manual build, `AutoBy`
  for an automatic one. Automatic builds therefore never need a per-revision approval for fetched commits, and the
  existing `needsApproval` logic stays unchanged. (Planning ruling: replaces "the setter is the author".)
- While a source is set, `PUT …/recipe` answers `409` with code `recipe_has_source`, and so does
  `POST …/recipe/edits` when it changes files; the CLI and web UI say so. References are not in the repository and
  stay editable through `recipe/edits` (references only): that saves a new revision with the same files and the same
  source string, needs `image:build` in addition to `image:edit`, and counts as a settings change like setting the
  source (the editor becomes `AutoBy`). Otherwise the next fetch, authored by `AutoBy`, would carry unapproved
  references of someone else into an automatic build. (Planning ruling: replaces "goes through the normal approval
  rule".)

### Fetching on build

At the start of each build of an image with a source, before a builder VM is requested:

1. Resolve the ref to a commit SHA (`GET api.github.com/repos/{repo}/commits/{ref}`, `Accept:
   application/vnd.github.sha`).
2. If the current revision was fetched from the same repo, path and SHA, build it unchanged (a build still runs so
   new base images arrive).
3. Otherwise download `GET api.github.com/repos/{repo}/tarball/{sha}`, take the files under `path`, apply the existing
   recipe checks (`Dockerfile` at the root, 50 MB, 1000 files, clean paths, file modes kept: executable bit),
   store them as a new revision with `source = github:{repo}@{sha}:{path}` and make it current. References are
   carried over from the previous revision.
4. Log each step into the build log: "fetching owner/repo@main → <sha7>", "unchanged since revision N" or "revision N
   from <sha7>".

Failures (repo or ref not found, no access, too large, no `Dockerfile`, GitHub unreachable) start no build and move
nothing: a manual build answers with the reason (`422`, or `502` when GitHub is unreachable); an automatic build sets
the image's "automatic rebuild failed" reason as today. The fetch happens before the build row exists, because a build
row references a revision. It is synchronous and bounded to 40 s (the hub's write timeout is 60 s). The log lines of a
successful fetch are written into the build's log right after the build row is created. (Planning ruling.)

Symlinks in the tarball: skipped with a log line (recipes have no symlinks today). Paths are taken relative to the
tarball's top-level `owner-repo-sha/` directory.

### Token

- The hub uses the GitHub integration of the person the build runs as (manual: who started it; automatic: `AutoBy`),
  resolved like references are allowed for the image's level: a private image may use that person's private, their
  team's or the org's integration, a team image the team's or the org's, an org image only the org's; narrowest
  first. The person needs `integration:use` on it.
- `vm` and `proxy` mode make no difference: the hub holds the real token.
- No integration available: fetch anonymously (public repos). A 404 then says "not found or no access; connect a
  GitHub integration".
- The token is only sent to `api.github.com` (the tarball redirect to `codeload.github.com` carries its own
  short-lived token; the Authorization header must not follow the redirect to another host). Never logged.

### Display

- Build list and build page show the commit (`sha7`, link to `https://github.com/{repo}/commit/{sha}`) for builds of a
  fetched revision.
- Versions keep pointing at the revision; the revision shows its source.

## Constraints

- Go hub, Postgres migrations numbered after the newest in the repo; store/API/CLI/web follow the existing image code
  (`internal/store/images.go`, `internal/api/images.go`, `internal/api/builds.go`, `internal/builds/scheduler.go`,
  `internal/api/autorebuild.go`, `cmd/observatory`).
- GitHub API calls go through the hub's existing outbound HTTP setup used for integration verification (timeouts,
  base URL overridable in tests).
- Docs: `docs/images.md` gets a "Recipes from GitHub" section (trust statement, token resolution, triggers, limits);
  `docs/cli.md` the new command; `docs/integrations.md` a line that the hub uses the GitHub token for image sources.

## Left to the builder

- Where exactly the fetch runs (in `startBuild`/`AutoRebuilds.start` before creating the build row, or as a first
  phase of the scheduler with the build row's revision filled in afterwards), as long as the log lines land in the
  build's log and the 60-minute timeout covers it.
- Table layout for the source (columns on the image vs. own table) and for the revision's source string.
- Web UI layout details within the existing image page.
