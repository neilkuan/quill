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

# Scenario 1: backup disabled (default values) — no init/sidecar
render "default" ""
grep -q 'name: s3-restore' "$TMP/default.yaml" \
    && fail "scenario 1: s3-restore should NOT render when backup is disabled"
grep -q 'name: s3-backup' "$TMP/default.yaml" \
    && fail "scenario 1: s3-backup should NOT render when backup is disabled"
grep -q 'fsGroup:' "$TMP/default.yaml" \
    && fail "scenario 1: pod-level fsGroup should not appear when backup is disabled"
pass "scenario 1: default values produce no rclone containers and no fsGroup"

# Scenario 2: IRSA — two-container layout, ownership, fsGroup, HOME=/tmp, IRSA SA
render "irsa" "$TESTS_DIR/values-irsa.yaml"
grep -q 'name: s3-restore' "$TMP/irsa.yaml" \
    || fail "scenario 2: s3-restore initContainer missing"
grep -q 'name: s3-backup' "$TMP/irsa.yaml" \
    || fail "scenario 2: s3-backup sidecar missing"
# Container ordering: s3-restore must come before s3-backup
[[ $(grep -n 'name: s3-restore\|name: s3-backup' "$TMP/irsa.yaml" | head -1) == *s3-restore* ]] \
    || fail "scenario 2: s3-restore must be declared before s3-backup"
# s3-backup must carry restartPolicy: Always; s3-restore must NOT
yq 'select(.kind == "Deployment").spec.template.spec.initContainers[]
    | select(.name == "s3-backup").restartPolicy' "$TMP/irsa.yaml" | grep -q 'Always' \
    || fail "scenario 2: s3-backup missing restartPolicy: Always"
yq 'select(.kind == "Deployment").spec.template.spec.initContainers[]
    | select(.name == "s3-restore").restartPolicy' "$TMP/irsa.yaml" | grep -qv 'Always' \
    || fail "scenario 2: s3-restore must not have restartPolicy: Always (it is a plain init)"
# s3-backup must carry lifecycle.preStop with rclone sync; s3-restore must not
yq 'select(.kind == "Deployment").spec.template.spec.initContainers[]
    | select(.name == "s3-backup").lifecycle.preStop.exec.command | join(" ")' "$TMP/irsa.yaml" \
    | grep -q 'rclone sync' \
    || fail "scenario 2: s3-backup missing rclone sync preStop"
yq 'select(.kind == "Deployment").spec.template.spec.initContainers[]
    | select(.name == "s3-restore").lifecycle' "$TMP/irsa.yaml" | grep -q 'null' \
    || fail "scenario 2: s3-restore must not have a lifecycle hook"
# Container command bodies — restore runs rclone copy (no sleep), backup sleeps (no rclone)
yq 'select(.kind == "Deployment").spec.template.spec.initContainers[]
    | select(.name == "s3-restore").command | join(" ")' "$TMP/irsa.yaml" \
    | grep -q 'rclone copy' \
    || fail "scenario 2: s3-restore command must contain 'rclone copy'"
yq 'select(.kind == "Deployment").spec.template.spec.initContainers[]
    | select(.name == "s3-restore").command | join(" ")' "$TMP/irsa.yaml" \
    | grep -q 'sleep' \
    && fail "scenario 2: s3-restore command must NOT contain 'sleep'"
yq 'select(.kind == "Deployment").spec.template.spec.initContainers[]
    | select(.name == "s3-backup").command | join(" ")' "$TMP/irsa.yaml" \
    | grep -q 'sleep infinity' \
    || fail "scenario 2: s3-backup command must contain 'sleep infinity'"
yq 'select(.kind == "Deployment").spec.template.spec.initContainers[]
    | select(.name == "s3-backup").command | join(" ")' "$TMP/irsa.yaml" \
    | grep -q 'rclone copy' \
    && fail "scenario 2: s3-backup command must NOT contain 'rclone copy' (only preStop runs sync)"
# Both containers run as 1000:1000 with runAsNonRoot
for c in s3-restore s3-backup; do
    uid=$(yq "select(.kind == \"Deployment\").spec.template.spec.initContainers[] | select(.name == \"$c\").securityContext.runAsUser" "$TMP/irsa.yaml")
    gid=$(yq "select(.kind == \"Deployment\").spec.template.spec.initContainers[] | select(.name == \"$c\").securityContext.runAsGroup" "$TMP/irsa.yaml")
    nonroot=$(yq "select(.kind == \"Deployment\").spec.template.spec.initContainers[] | select(.name == \"$c\").securityContext.runAsNonRoot" "$TMP/irsa.yaml")
    [[ "$uid" == "1000" ]] || fail "scenario 2: $c runAsUser should be 1000, got $uid"
    [[ "$gid" == "1000" ]] || fail "scenario 2: $c runAsGroup should be 1000, got $gid"
    [[ "$nonroot" == "true" ]] || fail "scenario 2: $c runAsNonRoot should be true, got $nonroot"
done
# Pod-level fsGroup
yq 'select(.kind == "Deployment").spec.template.spec.securityContext.fsGroup' "$TMP/irsa.yaml" | grep -q '^1000$' \
    || fail "scenario 2: pod-level securityContext.fsGroup should be 1000"
# HOME=/tmp on both rclone containers
for c in s3-restore s3-backup; do
    yq "select(.kind == \"Deployment\").spec.template.spec.initContainers[] | select(.name == \"$c\").env[] | select(.name == \"HOME\").value" "$TMP/irsa.yaml" | grep -q '^/tmp$' \
        || fail "scenario 2: $c missing HOME=/tmp env var"
done
# Existing IRSA assertions (image, SA, env, terminationGracePeriodSeconds)
grep -q 'image: "rclone/rclone:1.66"' "$TMP/irsa.yaml" \
    || fail "scenario 2: rclone wrong image"
grep -q 'kind: ServiceAccount' "$TMP/irsa.yaml" \
    || fail "scenario 2: ServiceAccount missing"
grep -q 'eks.amazonaws.com/role-arn: "arn:aws:iam::123456789012:role/quill-s3-backup"' "$TMP/irsa.yaml" \
    || fail "scenario 2: IRSA role annotation missing or wrong"
grep -q 'serviceAccountName: r-quill-kiro' "$TMP/irsa.yaml" \
    || fail "scenario 2: pod missing serviceAccountName"
grep -q 'value: "prod/quill/kiro"' "$TMP/irsa.yaml" \
    || fail "scenario 2: PREFIX env wrong value"
grep -q 'value: "my-quill-backups"' "$TMP/irsa.yaml" \
    || fail "scenario 2: BUCKET env wrong value"
grep -q 'terminationGracePeriodSeconds: 60' "$TMP/irsa.yaml" \
    || fail "scenario 2: terminationGracePeriodSeconds not 60"
grep -q 'name: AWS_ACCESS_KEY_ID' "$TMP/irsa.yaml" \
    && fail "scenario 2: IRSA mode should not inject AWS_ACCESS_KEY_ID env"
grep -q '\-s3-creds' "$TMP/irsa.yaml" \
    && fail "scenario 2: IRSA mode should not render s3-creds Secret"
pass "scenario 2: IRSA mode renders two-container layout + ownership + fsGroup + IRSA SA"

# Scenario 3: Pod Identity — sidecar + SA without IRSA annotation
render "pod-identity" "$TESTS_DIR/values-pod-identity.yaml"
grep -q 'name: s3-restore' "$TMP/pod-identity.yaml" \
    || fail "scenario 3: s3-restore initContainer missing"
grep -q 'name: s3-backup' "$TMP/pod-identity.yaml" \
    || fail "scenario 3: s3-backup sidecar missing"
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
grep -q 'name: s3-restore' "$TMP/secret-inline.yaml" \
    || fail "scenario 4: s3-restore initContainer missing"
grep -q 'name: s3-backup' "$TMP/secret-inline.yaml" \
    || fail "scenario 4: s3-backup sidecar missing"
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
grep -q 'name: s3-restore' "$TMP/secret-existing.yaml" \
    || fail "scenario 5: s3-restore initContainer missing"
grep -q 'name: s3-backup' "$TMP/secret-existing.yaml" \
    || fail "scenario 5: s3-backup sidecar missing"
grep -q 'r-quill-kiro-s3-creds' "$TMP/secret-existing.yaml" \
    && fail "scenario 5: chart should not create its own Secret when existingSecret is set"
grep -q 'name: my-precreated-aws-creds' "$TMP/secret-existing.yaml" \
    || fail "scenario 5: secretKeyRef does not point at the existing Secret"
pass "scenario 5: secret-existing mode references pre-existing Secret only"

echo
echo "All scenarios passed."
