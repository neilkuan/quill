# Releasing

## Overview

Releases are driven by `scripts/release.sh` and two GitHub Actions workflows. The core principle is **"what you tested is what you ship"** — stable releases promote pre-release images without rebuilding.

## Version Scheme

Versions follow SemVer with pre-release candidates:

- **Pre-release**: `v0.4.0-rc.1` — built and tested, tagged via `./scripts/release.sh --rc`
- **Stable**: `v0.4.0` — promotes a tested pre-release image (no rebuild)

## Release Script

```bash
./scripts/release.sh                    # auto-detect version bump, open Release PR
./scripts/release.sh --version 0.5.0    # specify version explicitly
./scripts/release.sh --rc               # create next RC tag (v0.4.0-rc.N)
./scripts/release.sh --dry-run          # preview what would happen
```

Version bump is auto-detected from conventional commits:
- `feat!:` or `BREAKING CHANGE:` → **major**
- `feat:` → **minor**
- `fix:`, `chore:`, `docs:`, etc. → **patch**

## Full Release Flow

```
  PR merged to main
        │
        ▼
  ┌──────────────────────┐     ┌──────────────────────────────┐
  │ release.yml          │────>│ Release PR                   │
  │ (auto or manual)     │     │ (branch: release/v0.4.1)     │
  └──────────────────────┘     │ (only changes VERSION file)  │
                               └──────────────────────────────┘

  git checkout release/v0.4.1
  ./scripts/release.sh --rc
        │
        ▼
  ┌─────────────┐     ┌──────────────────┐
  │ Creates tag │────>│ build.yml        │
  │ v0.4.1-rc.1│     │ (full build)     │
  └─────────────┘     └──────────────────┘
        │
        ▼
  Images tagged: <sha>, <version-rc.N>

  Merge Release PR
        │
        ▼
  ┌──────────────────┐     ┌──────────────────────────────┐
  │ tag-on-merge.yml │────>│ build.yml                    │
  │ tags v0.4.1      │     │ (promote, no rebuild)        │
  └──────────────────┘     └──────────────────────────────┘
        │
        ▼
  Images tagged: <version>, <major.minor>, latest
```

## Step by Step

1. **Develop** — merge feature/fix PRs to `main` as usual
2. **Release PR** — `release.yml` auto-opens a Release PR on push to main (or run `./scripts/release.sh` locally). This PR only changes the `VERSION` file — no code changes. The branch is named `release/v0.4.1`.
3. **RC tag** — checkout the release branch and create an RC tag:
   ```bash
   git fetch origin
   git checkout release/v0.4.1
   ./scripts/release.sh --rc
   ```
   This creates `v0.4.1-rc.1` on the release branch (where VERSION is already bumped to `0.4.1`) and pushes the tag, triggering a full Docker build via `build.yml`. If you need another RC, run `--rc` again to get `rc.2`, `rc.3`, etc.
4. **Test** — verify the RC images work correctly
5. **Ship** — merge the Release PR on GitHub → `tag-on-merge.yml` auto-creates stable tag `v0.4.1` → `build.yml` promotes the RC image to stable (no rebuild)

> **Why checkout the release branch for RC?** The `VERSION` file is bumped on the release branch (e.g., `0.4.0` → `0.4.1`), so `--rc` reads the correct version from there. The release branch only contains the VERSION bump — no code changes — so the built image is functionally identical to main.

## Image Tags

Each build produces five multi-arch image variants:

```
ghcr.io/neilkuan/openab-go:<tag>          # kiro-cli
ghcr.io/neilkuan/openab-go-codex:<tag>    # codex
ghcr.io/neilkuan/openab-go-claude:<tag>   # claude
ghcr.io/neilkuan/openab-go-gemini:<tag>   # gemini
ghcr.io/neilkuan/openab-go-copilot:<tag>  # GitHub Copilot CLI
```

Tag patterns:
- **Pre-release**: `<sha>`, `<version-rc.N>`
- **Stable**: `<version>`, `<major.minor>`, `latest`

## CI Workflows

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `release.yml` | Push to main / workflow_dispatch | Auto-open Release PR |
| `tag-on-merge.yml` | Release PR merged | Create stable git tag |
| `build.yml` | Any `v*` tag pushed | Build (RC) or promote (stable) Docker images |
| `test.yml` | PR / push to main | Go build, vet, test |

## Adding a new agent variant (e.g. copilot, qwen, …)

For every new agent CLI we support, a new image `ghcr.io/neilkuan/openab-go-<name>` has to be published. The flow below is mandatory — skipping the bootstrap step will make the first RC build fail with `denied: permission_denied: write_package`, which then cancels the whole matrix because `build.yml` uses registry cache (`cache-to: type=registry,ref=…:cache-<runner>`) that tries to write before the push is even attempted.

### 1. Code / config changes

1. Add `Dockerfile.<name>` — copy from `Dockerfile.claude` and keep the same 3-layer structure (system packages → pinned `gh` CLI → npm/apt agent packages) so registry cache reuse behaves the same way.
2. Add the variant to **all three matrix blocks** in `.github/workflows/build.yml`:
   - `build-image` — `{ suffix: "-<name>", dockerfile: "Dockerfile.<name>", artifact: "<name>" }`
   - `merge-manifests` — `{ suffix: "-<name>", artifact: "<name>" }`
   - `promote-stable` — `{ suffix: "-<name>" }`
3. Update image tables in `README.md`, `README-zh-tw.md`, `RELEASING.md`, and `CLAUDE.md`.
4. Add an `[agent]` example to `config.toml.example`.

### 2. Bootstrap the GHCR package (one-time, MUST happen before the first RC build)

User-owned GHCR packages (`/users/neilkuan/...`) must exist before the repo's `GITHUB_TOKEN` can push to them — they are **not auto-created on first push from Actions**. Seed the package by re-tagging any existing variant's multi-arch manifest:

```bash
# 1. Login to ghcr.io with a PAT that has write:packages
gh auth token | docker login ghcr.io -u neilkuan --password-stdin

# 2. Copy an existing multi-arch manifest to the new package name
#    (imagetools create preserves the manifest list — no need to pull/tag/push per arch)
docker buildx imagetools create \
  -t ghcr.io/neilkuan/openab-go-<name>:bootstrap \
  ghcr.io/neilkuan/openab-go-claude:<latest-stable>

# 3. Verify the package exists, is public, and linked to this repo
gh api /user/packages/container/openab-go-<name> \
  | jq '{name, visibility, repository: .repository.full_name}'
# expected: visibility=public, repository=neilkuan/openab-go
```

The `visibility` and `repository` fields are inherited from the source image's OCI labels (specifically `org.opencontainers.image.source`), so copying any of the existing public openab-go-* images sets them correctly. Bootstrap image content is irrelevant — it's overwritten by the first successful RC build.

### 3. Push the RC tag

From the matching `release/vX.Y.Z` branch:

```bash
git checkout release/vX.Y.Z
./scripts/release.sh --rc
```

All 5 variants × 2 platforms should build and push. If the new variant still fails with `write_package denied`, the repo needs to be given **Actions write access** to the new package:

- Open `https://github.com/users/neilkuan/packages/container/openab-go-<name>/settings`
- **Manage Actions access** → **Add repository** → `neilkuan/openab-go` → role `Write`

This setting is **not exposed by any REST/GraphQL API** — it must be done via the web UI exactly once per new package.

### 4. Clean up the bootstrap tag (optional)

The `:bootstrap` tag is only useful for the first push. Once the RC has produced `:<rc>` tags, delete the bootstrap version so it doesn't confuse future readers:

```bash
gh api -X DELETE \
  /user/packages/container/openab-go-<name>/versions/<version-id-of-bootstrap>
```

(Look up the version id via `gh api /user/packages/container/openab-go-<name>/versions | jq '.[] | select(.metadata.container.tags[] == "bootstrap")'`.)
