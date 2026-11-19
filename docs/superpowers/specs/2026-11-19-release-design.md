# Release pipeline

## Intent

Every push to `main` should produce a versioned release without manual steps: Commitizen picks the next version from the
conventional commits, the changelog is updated, binaries for macOS and Linux and the container images are built and
published as a GitHub release.

## Approaches

- **Plain workflow + build script + `gh release` (chosen):** a few dozen lines of shell next to the Makefile; the
  version only enters binaries through the existing `-ldflags`.
- GoReleaser: one more tool and config format for what a short script does here.
- Commitizen action vs. `pip install commitizen==<pinned>`: the pip install is pinned and has no extra action to trust.

## Design

- `.cz.toml`: `cz_conventional_commits`, tags `v$version`, `version_provider = "scm"` (the tag is the only place the
  version lives), `major_version_zero = true`, changelog `CHANGELOG.md` updated on bump. First release `v0.1.0`.
- `.github/workflows/release.yml` on push to `main` (concurrency `release`, never cancelled):
  1. `bump`: `cz bump --yes`, writing the version's changelog section to a file; exit code 21 (nothing to bump, e.g. only
     `docs:`/`chore:`) ends the run without a release. Pushes the bump commit and tag with `GITHUB_TOKEN`; pushes made
     with that token start no workflow, so there is no loop.
  2. `binaries`: `scripts/release-build.sh <version> dist/` builds the web GUI once, then `observatory` (CLI) and `hub`
     for linux/darwin × amd64/arm64 and `observatory-node` for linux/amd64, one `tar.gz` each, plus `SHA256SUMS`.
  3. `images`: `ghcr.io/asterism-dev/observatory-{hub,gateway,storage}` for linux/amd64 and linux/arm64, tagged with the
     version and `latest`, labelled with the repository as source. Go stages cross-compile on the build platform.
  4. `release`: `gh release create <tag>` with the changelog section as notes and every archive plus `SHA256SUMS`.
- `deploy/compose.yaml`: each built service gets `image: ghcr.io/asterism-dev/observatory-<name>:${OBSERVATORY_VERSION:-latest}`
  next to its `build:`; `up` pulls the published image, `up --build` still builds locally.

## Non-goals

Windows builds, signing/notarisation, Homebrew or apt packaging, release branches.
