# Releasing

Every push to `main` runs `.github/workflows/release.yml`:

1. **Bump:** Commitizen (`.cz.toml`) reads the conventional commits since the last `v*` tag. `feat` raises the minor
   version, `fix`/`perf`/`refactor` the patch version, a breaking change (`!` or `BREAKING CHANGE:`) the minor version
   while we are below 1.0. It prepends the section to `CHANGELOG.md`, commits `bump: version …` and pushes the commit
   and the tag. Only `docs`, `chore`, `ci`, `test`, `style` or `build` commits: the run ends without a release.
2. **Binaries:** `scripts/release-build.sh` builds `observatory` (CLI and services) for linux and darwin on amd64 and
   arm64, and `observatory-node` and `observatory-init` for linux/amd64, one `tar.gz` each, plus `SHA256SUMS`.
3. **Guest boot files:** the `guestboot` job builds `ghcr.io/asterism-dev/observatory-guestboot` (guest kernel,
   `mke2fs`, `resize2fs`) for linux/amd64, tagged with `GUESTBOOT_TAG` from `deploy/observatory.Dockerfile`. The kernel build is
   slow, so it runs only when that tag is not published yet; raise the tag to rebuild.
4. **Images:** `ghcr.io/asterism-dev/observatory` (edge and hub, `observatory serve` with `all` as the default
   argument) and `ghcr.io/asterism-dev/observatory-{gateway,storage}` for linux/amd64 and linux/arm64, and the guest
   and builder images `ghcr.io/asterism-dev/observatory-{guest,builder}` for linux/amd64, tagged with the version. The
   `observatory` image bundles the guest boot files of step 3.
5. **Release:** a GitHub release named after the tag, with the changelog section as text and the archives attached;
   then `latest` moves to this version's images.

The version lives only in the tag; binaries get it at build time (`observatory version` prints it). PR titles become
the squash commit messages on `main` (the repository squashes with the PR title and an empty body), so they decide the
next version: write them as conventional commits. Commitizen reads every line of a commit message, so a body line such
as `BREAKING CHANGE: …` would count too.

## Package visibility

A new GHCR package is private, and a workflow cannot change that with `GITHUB_TOKEN`. Nodes pull
`observatory-guest` anonymously, and hub builds outside the release workflow pull `observatory-guestboot`, so make
both public once, after the first release that publishes them:

1. On GitHub open the organization `asterism-dev` → **Packages** → `observatory-guest`.
2. **Package settings** → **Danger Zone** → **Change visibility** → **Public**, type the package name to confirm.
3. Repeat for `observatory-guestboot`.

GitHub has no REST endpoint to change the visibility, so this is a UI step. Check the result with
`gh api /orgs/asterism-dev/packages/container/observatory-guest --jq .visibility` (needs the `read:packages` scope)
or with an anonymous `docker buildx imagetools inspect ghcr.io/asterism-dev/observatory-guest:latest` after
`docker logout ghcr.io`.

## If a release fails

Once the bump job has pushed the tag, a failed binaries, images or release job leaves the tag without a GitHub release.
Use "Re-run failed jobs" on that run: it keeps the bump job's tag and notes. "Re-run all jobs" fails, because the bump
job would compute the same version again. The next push to `main` releases the next version, not the failed one.

Two merges in quick succession are fine: the push is atomic, so the older run fails without leaving a tag and the
queued run releases both. Branch protection or a ruleset on `main` must let GitHub Actions push, or the bump fails.

To build the archives locally (Linux, needs `sha256sum`): `make web && sh scripts/release-build.sh v0.0.0-dev dist`.
