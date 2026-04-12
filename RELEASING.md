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
  ┌──────────────────────┐     ┌──────────────────────┐
  │ release.yml          │────>│ Release PR            │
  │ (auto or manual)     │     │ (bumps VERSION)       │
  └──────────────────────┘     └──────────────────────┘

  ./scripts/release.sh --rc
        │
        ▼
  ┌─────────────┐     ┌──────────────────┐
  │ Creates tag │────>│ build.yml        │
  │ v0.4.0-rc.1│     │ (full build)     │
  └─────────────┘     └──────────────────┘
        │
        ▼
  Images tagged: <sha>, <version-rc.N>

  Merge Release PR
        │
        ▼
  ┌──────────────────┐     ┌──────────────────────────────┐
  │ tag-on-merge.yml │────>│ build.yml                    │
  │ tags v0.4.0      │     │ (promote, no rebuild)        │
  └──────────────────┘     └──────────────────────────────┘
        │
        ▼
  Images tagged: <version>, <major.minor>, latest
```

## Step by Step

1. **Develop** — merge feature/fix PRs to `main` as usual
2. **Release PR** — `release.yml` auto-opens a Release PR on push to main (or run `./scripts/release.sh` locally). This PR only changes the `VERSION` file — no code changes.
3. **RC tag** — on your local machine, checkout main and pull the latest:
   ```bash
   git checkout main
   git pull
   ./scripts/release.sh --rc
   ```
   This tags the current `main` HEAD (i.e., the commit that contains your feature/fix code) and pushes the tag, triggering a full Docker build via `build.yml`.
4. **Test** — verify the RC images work correctly
5. **Ship** — merge the Release PR on GitHub → `tag-on-merge.yml` auto-creates stable tag `v0.4.0` → `build.yml` promotes the RC image to stable (no rebuild)

> **Why does the RC tag go on main?** The Release PR only bumps the VERSION number — it contains zero code changes. The actual code you want to release is already on `main` after step 1. The stable release simply re-tags the already-tested RC image, so the code in the RC image and the stable image is identical.

## Image Tags

Each build produces four multi-arch image variants:

```
ghcr.io/neilkuan/openab-go:<tag>          # kiro-cli
ghcr.io/neilkuan/openab-go-codex:<tag>    # codex
ghcr.io/neilkuan/openab-go-claude:<tag>   # claude
ghcr.io/neilkuan/openab-go-gemini:<tag>   # gemini
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
