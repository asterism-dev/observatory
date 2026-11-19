# Releasing

Every push to `main` runs `.github/workflows/release.yml`:

1. **Bump:** Commitizen (`.cz.toml`) reads the conventional commits since the last `v*` tag. `feat` raises the minor
   version, `fix`/`perf`/`refactor` the patch version, a breaking change (`!` or `BREAKING CHANGE:`) the minor version
   while we are below 1.0. It prepends the section to `CHANGELOG.md`, commits `bump: version …` and pushes the commit
   and the tag. Only `docs`, `chore`, `ci`, `test`, `style` or `build` commits: the run ends without a release.
2. **Binaries:** `scripts/release-build.sh` builds `observatory` (CLI) and `hub` for linux and darwin on amd64 and
   arm64, and `observatory-node` for linux/amd64, one `tar.gz` each, plus `SHA256SUMS`.
3. **Images:** `ghcr.io/asterism-dev/observatory-{hub,gateway,storage}` for linux/amd64 and linux/arm64, tagged with the
   version and `latest`.
4. **Release:** a GitHub release named after the tag, with the changelog section as text and the archives attached.

The version lives only in the tag; binaries get it at build time (`observatory version` prints it). PR titles become
the squash commit messages on `main`, so they decide the next version: write them as conventional commits.

To build the archives locally (Linux, needs `sha256sum`): `make web && sh scripts/release-build.sh v0.0.0-dev dist`.
