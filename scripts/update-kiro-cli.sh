#!/usr/bin/env bash
# Bump the pinned kiro-cli version + SHA256 hashes in Dockerfile.
#
# Reads the current latest version from AWS's published manifest, pulls the
# per-zip SHA256 files for both architectures, and rewrites the three ARGs in
# Dockerfile. Run manually when you want to upgrade kiro-cli.
#
# Usage: scripts/update-kiro-cli.sh [VERSION]
#   VERSION — optional, defaults to the latest published version.

set -euo pipefail

BASE="https://desktop-release.q.us-east-1.amazonaws.com"
DOCKERFILE="$(git rev-parse --show-toplevel)/Dockerfile"

if [[ $# -ge 1 ]]; then
  VERSION="$1"
else
  VERSION=$(curl -sSfL "${BASE}/latest/manifest.json" | awk -F'"' '/"version":/ {print $4; exit}')
fi

if [[ -z "${VERSION}" ]]; then
  echo "error: could not resolve kiro-cli version" >&2
  exit 1
fi

echo "Pinning kiro-cli to ${VERSION}"

SHA_AMD64=$(curl -sSfL "${BASE}/${VERSION}/kirocli-x86_64-linux.zip.sha256")
SHA_ARM64=$(curl -sSfL "${BASE}/${VERSION}/kirocli-aarch64-linux.zip.sha256")

if [[ -z "${SHA_AMD64}" || -z "${SHA_ARM64}" ]]; then
  echo "error: failed to fetch SHA256 for ${VERSION}" >&2
  exit 1
fi

echo "  x86_64:   ${SHA_AMD64}"
echo "  aarch64:  ${SHA_ARM64}"

# In-place rewrite; BSD sed (macOS) and GNU sed both accept -i '' on macOS
# via the empty-extension form, so portability-safe form:
tmp=$(mktemp)
awk -v ver="${VERSION}" -v amd="${SHA_AMD64}" -v arm="${SHA_ARM64}" '
  /^ARG KIRO_CLI_VERSION=/        { print "ARG KIRO_CLI_VERSION=" ver; next }
  /^ARG KIRO_CLI_SHA256_AMD64=/   { print "ARG KIRO_CLI_SHA256_AMD64=" amd; next }
  /^ARG KIRO_CLI_SHA256_ARM64=/   { print "ARG KIRO_CLI_SHA256_ARM64=" arm; next }
  { print }
' "${DOCKERFILE}" > "${tmp}"
mv "${tmp}" "${DOCKERFILE}"

echo "Updated ${DOCKERFILE}"
