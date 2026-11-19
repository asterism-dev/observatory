# Releasing

Every push to `main` runs `.github/workflows/release.yml`:

1. **Bump:** Commitizen (`.cz.toml`) reads the conventional commits since the last `v*` tag. `feat` raises the minor
   version, `fix`/`perf`/`refactor` the patch version, a breaking change (`!` or `BREAKING CHANGE:`) the minor version
   while we are below 1.0. It prepends the section to `CHANGELOG.md`, commits `bump: version …` and pushes the commit
   and the tag. Only `docs`, `chore`, `ci`, `test`, `style` or `build` commits: the run ends without a release.
2. **Binaries:** `scripts/release-build.sh` builds `observatory` (CLI) and `hub` for linux and darwin on amd64 and
   arm64, and `observatory-node` for linux/amd64, one `tar.gz` each, plus `SHA256SUMS`.
3. **Images:** `ghcr.io/asterism-dev/observatory-{hub,gateway,storage}` for linux/amd64 and linux/arm64, tagged with the
   version.
4. **Release:** a GitHub release named after the tag, with the changelog section as text and the archives attached;
   then `latest` moves to this version's images.

The version lives only in the tag; binaries get it at build time (`observatory version` prints it). PR titles become
the squash commit messages on `main` (the repository squashes with the PR title and an empty body), so they decide the
next version: write them as conventional commits. Commitizen reads every line of a commit message, so a body line such
as `BREAKING CHANGE: …` would count too.

## If a release fails

Once the bump job has pushed the tag, a failed binaries, images or release job leaves the tag without a GitHub release.
Use "Re-run failed jobs" on that run: it keeps the bump job's tag and notes. "Re-run all jobs" fails, because the bump
job would compute the same version again. The next push to `main` releases the next version, not the failed one.

Two merges in quick succession are fine: the push is atomic, so the older run fails without leaving a tag and the
queued run releases both. Branch protection or a ruleset on `main` must let GitHub Actions push, or the bump fails.

To build the archives locally (Linux, needs `sha256sum`): `make web && sh scripts/release-build.sh v0.0.0-dev dist`.
