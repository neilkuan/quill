#!/usr/bin/env bash
# Hermetic chart-template tests. Each scenario renders the chart with a
# values fixture and greps the output for invariants.
#
# Usage: bash deploy/helm/quill/tests/template-tests.sh
set -euo pipefail

CHART_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TESTS_DIR="$CHART_DIR/tests"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

pass() { printf '\033[32mOK\033[0m  %s\n' "$1"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$1" >&2; exit 1; }

render() {
    local name="$1" values="$2"
    if [[ -n "$values" ]]; then
        helm template r "$CHART_DIR" -f "$values" > "$TMP/$name.yaml"
    else
        helm template r "$CHART_DIR" > "$TMP/$name.yaml"
    fi
}

# Scenario 1: backup disabled (default values) — no sidecar
render "default" ""
grep -q 'name: s3-sync' "$TMP/default.yaml" \
    && fail "scenario 1: sidecar should NOT render when backup is disabled"
pass "scenario 1: default values produce no s3-sync sidecar"

# Scenario 2: IRSA — sidecar present, ServiceAccount carries IRSA annotation, no inline Secret
render "irsa" "$TESTS_DIR/values-irsa.yaml"
grep -q 'name: s3-sync' "$TMP/irsa.yaml" \
    || fail "scenario 2: sidecar missing"
grep -q 'restartPolicy: Always' "$TMP/irsa.yaml" \
    || fail "scenario 2: sidecar missing restartPolicy: Always"
grep -q 'image: "rclone/rclone:1.66"' "$TMP/irsa.yaml" \
    || fail "scenario 2: sidecar wrong image"
grep -q 'kind: ServiceAccount' "$TMP/irsa.yaml" \
    || fail "scenario 2: ServiceAccount missing"
grep -q 'eks.amazonaws.com/role-arn: "arn:aws:iam::123456789012:role/quill-s3-backup"' "$TMP/irsa.yaml" \
    || fail "scenario 2: IRSA role annotation missing or wrong"
grep -q 'serviceAccountName: r-quill-kiro' "$TMP/irsa.yaml" \
    || fail "scenario 2: pod missing serviceAccountName"
grep -q 'name: PREFIX' "$TMP/irsa.yaml" \
    || fail "scenario 2: PREFIX env missing"
grep -q 'value: "prod/quill/kiro"' "$TMP/irsa.yaml" \
    || fail "scenario 2: PREFIX env wrong value"
grep -q 'name: BUCKET' "$TMP/irsa.yaml" \
    || fail "scenario 2: BUCKET env missing"
grep -q 'value: "my-quill-backups"' "$TMP/irsa.yaml" \
    || fail "scenario 2: BUCKET env wrong value"
grep -q 'terminationGracePeriodSeconds: 60' "$TMP/irsa.yaml" \
    || fail "scenario 2: terminationGracePeriodSeconds not bumped to 60"
# IRSA mode must NOT inject AWS_ACCESS_KEY_ID env var
grep -q 'name: AWS_ACCESS_KEY_ID' "$TMP/irsa.yaml" \
    && fail "scenario 2: IRSA mode should not inject AWS_ACCESS_KEY_ID env"
# IRSA mode must NOT render a chart-managed s3-creds Secret
grep -q '\-s3-creds' "$TMP/irsa.yaml" \
    && fail "scenario 2: IRSA mode should not render s3-creds Secret"
pass "scenario 2: IRSA mode renders sidecar + SA + IRSA annotation"

# Scenario 3: Pod Identity — sidecar + SA without IRSA annotation
render "pod-identity" "$TESTS_DIR/values-pod-identity.yaml"
grep -q 'name: s3-sync' "$TMP/pod-identity.yaml" \
    || fail "scenario 3: sidecar missing"
grep -q 'kind: ServiceAccount' "$TMP/pod-identity.yaml" \
    || fail "scenario 3: ServiceAccount missing"
grep -q 'eks.amazonaws.com/role-arn' "$TMP/pod-identity.yaml" \
    && fail "scenario 3: Pod Identity mode should NOT render IRSA annotation"
grep -q 'serviceAccountName: r-quill-kiro' "$TMP/pod-identity.yaml" \
    || fail "scenario 3: pod missing serviceAccountName"
grep -q 'name: AWS_ACCESS_KEY_ID' "$TMP/pod-identity.yaml" \
    && fail "scenario 3: Pod Identity mode should not inject AWS_ACCESS_KEY_ID env"
grep -q '\-s3-creds' "$TMP/pod-identity.yaml" \
    && fail "scenario 3: Pod Identity mode should not render s3-creds Secret"
pass "scenario 3: Pod Identity mode renders sidecar + bare SA"

# Scenario 4: Secret mode (inline) — sidecar + chart-managed Secret + valueFrom env, no SA
render "secret-inline" "$TESTS_DIR/values-secret-inline.yaml"
grep -q 'name: s3-sync' "$TMP/secret-inline.yaml" \
    || fail "scenario 4: sidecar missing"
grep -q 'r-quill-kiro-s3-creds' "$TMP/secret-inline.yaml" \
    || fail "scenario 4: chart-managed Secret r-quill-kiro-s3-creds not rendered"
grep -q 'AKIAEXAMPLEEXAMPLE12' "$TMP/secret-inline.yaml" \
    && fail "scenario 4: access key should be base64-encoded, not plaintext"
# AKIAEXAMPLEEXAMPLE12 base64-encoded
grep -q 'AWS_ACCESS_KEY_ID:' "$TMP/secret-inline.yaml" \
    || fail "scenario 4: Secret missing AWS_ACCESS_KEY_ID key"
grep -q 'name: AWS_ACCESS_KEY_ID' "$TMP/secret-inline.yaml" \
    || fail "scenario 4: sidecar env missing AWS_ACCESS_KEY_ID"
grep -q 'name: r-quill-kiro-s3-creds' "$TMP/secret-inline.yaml" \
    || fail "scenario 4: secretKeyRef points at wrong Secret name"
# Static-credential mode does NOT need a ServiceAccount
grep -q 'kind: ServiceAccount' "$TMP/secret-inline.yaml" \
    && fail "scenario 4: secret-inline mode should not render ServiceAccount"
grep -q 'serviceAccountName:' "$TMP/secret-inline.yaml" \
    && fail "scenario 4: secret-inline mode should not set serviceAccountName"
pass "scenario 4: secret-inline mode renders sidecar + chart-managed Secret"

# Scenario 5: Secret mode (existing) — sidecar references existing Secret, no chart-managed Secret rendered
render "secret-existing" "$TESTS_DIR/values-secret-existing.yaml"
grep -q 'name: s3-sync' "$TMP/secret-existing.yaml" \
    || fail "scenario 5: sidecar missing"
grep -q 'r-quill-kiro-s3-creds' "$TMP/secret-existing.yaml" \
    && fail "scenario 5: chart should not create its own Secret when existingSecret is set"
grep -q 'name: my-precreated-aws-creds' "$TMP/secret-existing.yaml" \
    || fail "scenario 5: secretKeyRef does not point at the existing Secret"
pass "scenario 5: secret-existing mode references pre-existing Secret only"

echo
echo "All scenarios passed."
