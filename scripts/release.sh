#!/usr/bin/env bash
set -euo pipefail

# release.sh — Prepare stable release PR or create RC tags.
#
# Usage:
#   ./scripts/release.sh                    # auto-detect version bump, open Release PR
#   ./scripts/release.sh --version 0.4.0    # specify version explicitly
#   ./scripts/release.sh --rc               # create next RC tag (v0.4.0-rc.N)
#   ./scripts/release.sh --dry-run          # print what would happen, don't execute
#   ./scripts/release.sh --rc --dry-run     # preview RC tag

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERSION_FILE="$REPO_ROOT/VERSION"

# --- Argument parsing ---
MODE="stable"
DRY_RUN=false
MANUAL_VERSION=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --rc)       MODE="rc"; shift ;;
    --dry-run)  DRY_RUN=true; shift ;;
    --version)  MANUAL_VERSION="$2"; shift 2 ;;
    --version=*)MANUAL_VERSION="${1#*=}"; shift ;;
    -h|--help)
      echo "Usage: $0 [--rc] [--version X.Y.Z] [--dry-run]"
      echo ""
      echo "Modes:"
      echo "  (default)   Prepare a stable Release PR (bump VERSION, create branch, open PR)"
      echo "  --rc        Create next RC tag (e.g., v0.4.0-rc.1) on current commit"
      echo ""
      echo "Options:"
      echo "  --version   Override auto-detected version (stable mode only)"
      echo "  --dry-run   Print what would happen without making changes"
      exit 0
      ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
done

# --- Helpers ---
info()  { echo "==> $*"; }
warn()  { echo "⚠️  $*" >&2; }
die()   { echo "❌ $*" >&2; exit 1; }
dry()   { if $DRY_RUN; then echo "[dry-run] $*"; else "$@"; fi; }

current_version() {
  [[ -f "$VERSION_FILE" ]] || die "VERSION file not found: $VERSION_FILE"
  cat "$VERSION_FILE" | tr -d '[:space:]'
}

last_stable_tag() {
  git tag -l 'v[0-9]*' --sort=-v:refname | grep -vE '-' | head -1
}

# Bump a semver component: bump_version <current> <part>
# part = major | minor | patch
bump_version() {
  local ver="$1" part="$2"
  local major minor patch
  IFS='.' read -r major minor patch <<< "$ver"

  case "$part" in
    major) echo "$((major + 1)).0.0" ;;
    minor) echo "${major}.$((minor + 1)).0" ;;
    patch) echo "${major}.${minor}.$((patch + 1))" ;;
    *) die "Unknown bump part: $part" ;;
  esac
}

# Detect bump type from conventional commits since a tag
detect_bump() {
  local since_tag="$1"
  local range="${since_tag}..HEAD"

  # Check commit subjects and bodies
  local subjects bodies
  subjects="$(git log "$range" --format=%s 2>/dev/null || true)"
  bodies="$(git log "$range" --format=%b 2>/dev/null || true)"

  if echo "$subjects" | grep -qiE '^feat(\(.+\))?!:' || echo "$bodies" | grep -qiE '^BREAKING CHANGE:'; then
    echo "major"
  elif echo "$subjects" | grep -qiE '^feat(\(.+\))?:'; then
    echo "minor"
  else
    echo "patch"
  fi
}

# --- RC Mode ---
do_rc() {
  local ver
  ver="$(current_version)"
  info "Current VERSION: $ver"

  # Find existing RC tags for this version
  local latest_rc next_num
  latest_rc="$(git tag -l "v${ver}-rc.*" --sort=-v:refname | head -1)"

  if [[ -z "$latest_rc" ]]; then
    next_num=1
  else
    # Extract rc number: v0.4.0-rc.3 → 3
    local current_num
    current_num="${latest_rc##*-rc.}"
    next_num=$((current_num + 1))
  fi

  local tag="v${ver}-rc.${next_num}"
  info "Next RC tag: $tag"

  if git rev-parse "$tag" >/dev/null 2>&1; then
    die "Tag $tag already exists"
  fi

  if $DRY_RUN; then
    echo "[dry-run] git tag $tag"
    echo "[dry-run] git push origin $tag"
    echo ""
    info "Would create and push tag: $tag"
    return
  fi

  git tag "$tag"
  git push origin "$tag"

  info "Created and pushed tag: $tag"
  info "build.yml will trigger pre-release build for $tag"
}

# --- Stable Release Mode ---
do_stable() {
  # Verify we're on main and up to date
  local branch
  branch="$(git rev-parse --abbrev-ref HEAD)"
  if [[ "$branch" != "main" ]]; then
    die "Must be on 'main' branch (currently on '$branch')"
  fi

  git fetch origin main --tags --quiet
  local local_sha remote_sha
  local_sha="$(git rev-parse HEAD)"
  remote_sha="$(git rev-parse origin/main)"
  if [[ "$local_sha" != "$remote_sha" ]]; then
    die "Local main ($local_sha) differs from origin/main ($remote_sha). Run 'git pull' first."
  fi

  local cur_ver last_tag new_ver
  cur_ver="$(current_version)"
  last_tag="$(last_stable_tag)"
  info "Current VERSION: $cur_ver"
  info "Last stable tag: ${last_tag:-"(none)"}"

  # Check if there are new commits
  if [[ -n "$last_tag" ]]; then
    local commit_count
    commit_count="$(git rev-list "${last_tag}..HEAD" --count)"
    if [[ "$commit_count" -eq 0 ]]; then
      info "No new commits since $last_tag — nothing to release."
      exit 0
    fi
    info "Commits since $last_tag: $commit_count"
  fi

  # Determine new version
  if [[ -n "$MANUAL_VERSION" ]]; then
    new_ver="$MANUAL_VERSION"
    info "Using manual version: $new_ver"
  else
    local bump_type
    bump_type="$(detect_bump "${last_tag:-HEAD~100}")"
    new_ver="$(bump_version "$cur_ver" "$bump_type")"
    info "Detected bump: $bump_type → $new_ver"
  fi

  # Check if tag already exists
  if git rev-parse "v${new_ver}" >/dev/null 2>&1; then
    die "Tag v${new_ver} already exists"
  fi

  local release_branch="release/v${new_ver}"

  # Check if branch already exists
  if git rev-parse --verify "$release_branch" >/dev/null 2>&1; then
    die "Branch $release_branch already exists locally"
  fi
  if git ls-remote --exit-code origin "refs/heads/$release_branch" >/dev/null 2>&1; then
    die "Branch $release_branch already exists on remote"
  fi

  # Check if there's already an open Release PR
  local open_prs
  open_prs="$(gh pr list --base main --state open --json headRefName --jq '.[].headRefName' 2>/dev/null | grep '^release/' || true)"
  if [[ -n "$open_prs" ]]; then
    warn "Open Release PR already exists: $open_prs"
    die "Close or merge the existing Release PR first."
  fi

  if $DRY_RUN; then
    echo ""
    echo "[dry-run] Would update VERSION: $cur_ver → $new_ver"
    echo "[dry-run] git checkout -b $release_branch"
    echo "[dry-run] git add VERSION && git commit -m 'release: prepare v$new_ver'"
    echo "[dry-run] git push -u origin $release_branch"
    echo "[dry-run] gh pr create --base main --title 'Release v$new_ver'"
    return
  fi

  # Execute
  echo "$new_ver" > "$VERSION_FILE"
  git checkout -b "$release_branch"
  git add "$VERSION_FILE"
  git commit -m "release: prepare v${new_ver}"
  git push -u origin "$release_branch"

  gh pr create \
    --base main \
    --head "$release_branch" \
    --title "Release v${new_ver}" \
    --body "$(cat <<EOF
Automated release PR for **v${new_ver}**.

When merged, a stable tag \`v${new_ver}\` will be created automatically, triggering the Docker image promotion workflow.

##### Pre-release checklist
- [ ] Create RC tag: \`./scripts/release.sh --rc\`
- [ ] Verify RC images build successfully
- [ ] Test RC images
EOF
)"

  info "Release PR created for v${new_ver}"
  info "Next step: ./scripts/release.sh --rc (to create RC tag for testing)"
}

# --- Main ---
case "$MODE" in
  rc)     do_rc ;;
  stable) do_stable ;;
esac
