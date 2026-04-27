# Helm S3 Backup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in S3 backup mechanism to `deploy/helm/quill/` so each instance's `agent.workingDir` (`/home/agent` or `/home/node`) is restored from S3 on pod start and synced back on graceful shutdown — preserving session continuity across pod restarts.

**Architecture:** A k8s 1.28+ native sidecar (`restartPolicy: Always`) runs `rclone/rclone:1.66`. Sidecar startup performs `rclone copy s3:... /workdir`; sidecar `preStop` hook performs `rclone sync /workdir s3:...`. K8s 1.28+ guarantees sidecar `preStop` fires *after* the main container exits, giving a clean ordering. Three AWS auth modes (IRSA / EKS Pod Identity / static credentials) are exposed via `values.yaml` toggle; rclone uses `env_auth=true` so the same sidecar config works for all three.

**Tech Stack:** Helm 3 / 4 templates, Kubernetes 1.28+ native sidecars, rclone S3 backend, AWS IAM (IRSA, Pod Identity), Bash for hermetic chart-template tests.

**Spec:** [`docs/superpowers/specs/2026-04-27-helm-s3-backup-design.md`](../specs/2026-04-27-helm-s3-backup-design.md)

---

## File Plan

**New files**

- `deploy/helm/quill/templates/serviceaccount.yaml` — Per-instance ServiceAccount; carries `eks.amazonaws.com/role-arn` annotation when `auth.mode=irsa`.
- `deploy/helm/quill/templates/secret-s3.yaml` — Per-instance `Secret` for inline static credentials. Rendered only when `auth.mode=secret` and `existingSecret` is unset.
- `deploy/helm/quill/tests/template-tests.sh` — Bash hermetic test runner. Runs `helm template` against five values fixtures and asserts on the rendered output via `grep`.
- `deploy/helm/quill/tests/values-irsa.yaml` — Fixture: backup enabled, IRSA mode.
- `deploy/helm/quill/tests/values-pod-identity.yaml` — Fixture: backup enabled, Pod Identity mode.
- `deploy/helm/quill/tests/values-secret-inline.yaml` — Fixture: backup enabled, secret mode with inline keys.
- `deploy/helm/quill/tests/values-secret-existing.yaml` — Fixture: backup enabled, secret mode pointing at a pre-existing Secret.

**Modified files**

- `deploy/helm/quill/values.yaml` — Add `backup` block (disabled by default), per-instance `backup` override hook.
- `deploy/helm/quill/Chart.yaml` — Bump `version` 0.1.0 → 0.2.0; add `kubeVersion: ">=1.28.0-0"`.
- `deploy/helm/quill/templates/deployment.yaml` — Add the native sidecar (`restartPolicy: Always`) block inside `initContainers`; bump `terminationGracePeriodSeconds` from 30 to 60; wire `serviceAccountName` when backup enabled.
- `deploy/helm/quill/README.md` — New "Session backup" section: K8s 1.28+ requirement, three auth-mode walkthroughs, `replicas: 1` constraint reminder, AWS S3 lifecycle pointer.

**Untouched** (sanity checklist)

- All `Dockerfile*` variants — sidecar-based design avoids touching agent images.
- Go source under `acp/`, `command/`, `discord/`, `telegram/`, `teams/` — feature lives entirely in chart templates.
- Existing `templates/configmap.yaml` / `templates/secret.yaml` (the platform-tokens secret) / `templates/ingress.yaml` / `templates/service.yaml` — no changes.

---

## Task 1: Hermetic chart-template test harness + values defaults + Chart.yaml bumps

**Goal:** Stand up the test infrastructure that all subsequent tasks will assert against. Ship it with one passing assertion ("backup disabled by default produces no sidecar"), so the harness is proven before we add more scenarios. Also land the values defaults and Chart.yaml metadata bumps in the same commit — both are foundational.

**Files:**
- Create: `deploy/helm/quill/tests/template-tests.sh`
- Modify: `deploy/helm/quill/values.yaml`
- Modify: `deploy/helm/quill/Chart.yaml`

- [ ] **Step 1: Write the failing test harness**

Create `deploy/helm/quill/tests/template-tests.sh`:

```bash
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

echo
echo "All scenarios passed."
```

Make it executable:

```bash
chmod +x deploy/helm/quill/tests/template-tests.sh
```

- [ ] **Step 2: Run test to verify the harness works against the current chart**

```
bash deploy/helm/quill/tests/template-tests.sh
```

Expected output:

```
OK  scenario 1: default values produce no s3-sync sidecar

All scenarios passed.
```

The test passes immediately because the chart currently has no sidecar at all. This step confirms `helm template` works and the harness scaffolding is sound.

- [ ] **Step 3: Add `backup` block to `values.yaml`**

Append to `deploy/helm/quill/values.yaml` (after the existing `instances` block, before `pool`):

```yaml
# -- Optional S3 backup of agent.workingDir for session continuity.
# Restores on pod start, syncs on graceful shutdown.
# Requires Kubernetes 1.28+ (native sidecar containers).
# Per-instance overrides via `instances.<name>.backup` merge over this default.
backup:
  enabled: false
  s3:
    bucket: ""
    prefix: "quill"        # final S3 path: s3://{bucket}/{prefix}/{instance-name}
    region: us-east-1
    endpoint: ""           # optional, for MinIO / non-AWS S3-compatible
  auth:
    # mode: irsa | podIdentity | secret
    mode: irsa
    irsa:
      roleArn: ""          # set when mode=irsa
    podIdentity: {}        # No fields — association created out-of-band
    secret:
      existingSecret: ""   # name of pre-existing k8s Secret with AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY
      accessKeyID: ""      # used only when existingSecret is empty
      secretAccessKey: ""
  rclone:
    image: rclone/rclone
    tag: "1.66"
    pullPolicy: IfNotPresent
    extraArgs: []          # passed to both copy (restore) and sync (backup)
  resources:
    requests:
      cpu: 10m
      memory: 32Mi
    limits:
      cpu: 100m
      memory: 64Mi
```

- [ ] **Step 4: Bump `Chart.yaml`**

Edit `deploy/helm/quill/Chart.yaml`:

```yaml
apiVersion: v2
name: quill
description: ACP bridge for Discord, Telegram, and Microsoft Teams
type: application
version: 0.2.0
appVersion: ""
kubeVersion: ">=1.28.0-0"
```

- [ ] **Step 5: Re-run the harness — must still pass**

```
bash deploy/helm/quill/tests/template-tests.sh
```

Expected: same `OK scenario 1` output. The new `backup` defaults do not flip `enabled: true`, so no sidecar renders.

- [ ] **Step 6: Lint the chart**

```
helm lint deploy/helm/quill
```

Expected: `1 chart(s) linted, 0 chart(s) failed` (or equivalent for Helm 4).

- [ ] **Step 7: Commit**

```bash
git add deploy/helm/quill/Chart.yaml deploy/helm/quill/values.yaml deploy/helm/quill/tests/template-tests.sh
git commit -m "feat(helm): scaffold S3 backup defaults and template-test harness"
```

---

## Task 2: IRSA auth mode

**Goal:** Implement the sidecar block in `deployment.yaml`, the per-instance `ServiceAccount` with the IRSA annotation, and prove via a new test scenario that the rendered output looks right.

**Files:**
- Create: `deploy/helm/quill/templates/serviceaccount.yaml`
- Create: `deploy/helm/quill/tests/values-irsa.yaml`
- Modify: `deploy/helm/quill/templates/deployment.yaml`
- Modify: `deploy/helm/quill/tests/template-tests.sh`

- [ ] **Step 1: Add the IRSA test fixture**

Create `deploy/helm/quill/tests/values-irsa.yaml`:

```yaml
backup:
  enabled: true
  s3:
    bucket: my-quill-backups
    prefix: prod/quill
    region: us-east-1
  auth:
    mode: irsa
    irsa:
      roleArn: arn:aws:iam::123456789012:role/quill-s3-backup
```

- [ ] **Step 2: Append the IRSA scenario to the test harness**

In `deploy/helm/quill/tests/template-tests.sh`, append before the final "All scenarios passed." line:

```bash
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
```

- [ ] **Step 3: Run the harness — confirm scenario 2 fails**

```
bash deploy/helm/quill/tests/template-tests.sh
```

Expected: `FAIL scenario 2: sidecar missing` — the sidecar template does not exist yet.

- [ ] **Step 4: Create `templates/serviceaccount.yaml`**

```yaml
{{- range $name, $inst := .Values.instances }}
{{- if $inst.enabled }}
{{- $b := merge (default (dict) $inst.backup) $.Values.backup }}
{{- if $b.enabled }}
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "quill.fullname" $ }}-{{ $name }}
  labels:
    {{- include "quill.labels" $ | nindent 4 }}
    app.kubernetes.io/component: {{ $name }}
  {{- if eq $b.auth.mode "irsa" }}
  annotations:
    eks.amazonaws.com/role-arn: {{ $b.auth.irsa.roleArn | quote }}
  {{- end }}
{{- end }}
{{- end }}
{{- end }}
```

- [ ] **Step 5: Modify `templates/deployment.yaml` to add the sidecar block**

Replace the entire file with:

```yaml
{{- range $name, $inst := .Values.instances }}
{{- if $inst.enabled }}
{{- $b := merge (default (dict) $inst.backup) $.Values.backup }}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "quill.fullname" $ }}-{{ $name }}
  labels:
    {{- include "quill.labels" $ | nindent 4 }}
    app.kubernetes.io/component: {{ $name }}
spec:
  replicas: 1
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
  selector:
    matchLabels:
      {{- include "quill.selectorLabels" $ | nindent 6 }}
      app.kubernetes.io/component: {{ $name }}
  template:
    metadata:
      labels:
        {{- include "quill.selectorLabels" $ | nindent 8 }}
        app.kubernetes.io/component: {{ $name }}
      annotations:
        checksum/config: {{ toJson $inst | sha256sum }}
    spec:
      terminationGracePeriodSeconds: 60
      {{- if and $b.enabled (or (eq $b.auth.mode "irsa") (eq $b.auth.mode "podIdentity")) }}
      serviceAccountName: {{ include "quill.fullname" $ }}-{{ $name }}
      {{- end }}
      {{- if $b.enabled }}
      initContainers:
        - name: s3-sync
          image: "{{ $b.rclone.image }}:{{ $b.rclone.tag }}"
          imagePullPolicy: {{ $b.rclone.pullPolicy }}
          restartPolicy: Always
          command:
            - /bin/sh
            - -c
            - |
              set -eu
              rclone copy "s3:${BUCKET}/${PREFIX}" /workdir --create-empty-src-dirs $EXTRA_ARGS || true
              while true; do sleep 3600; done
          lifecycle:
            preStop:
              exec:
                command:
                  - /bin/sh
                  - -c
                  - |
                    set -eu
                    rclone sync /workdir "s3:${BUCKET}/${PREFIX}" --create-empty-src-dirs $EXTRA_ARGS
          env:
            - name: BUCKET
              value: {{ $b.s3.bucket | quote }}
            - name: PREFIX
              value: "{{ $b.s3.prefix }}/{{ $name }}"
            - name: RCLONE_CONFIG_S3_TYPE
              value: s3
            - name: RCLONE_CONFIG_S3_PROVIDER
              value: AWS
            - name: RCLONE_CONFIG_S3_REGION
              value: {{ $b.s3.region | quote }}
            - name: RCLONE_CONFIG_S3_ENV_AUTH
              value: "true"
            {{- if $b.s3.endpoint }}
            - name: RCLONE_CONFIG_S3_ENDPOINT
              value: {{ $b.s3.endpoint | quote }}
            {{- end }}
            - name: EXTRA_ARGS
              value: {{ join " " $b.rclone.extraArgs | quote }}
            {{- if eq $b.auth.mode "secret" }}
            - name: AWS_ACCESS_KEY_ID
              valueFrom:
                secretKeyRef:
                  name: {{ $b.auth.secret.existingSecret | default (printf "%s-%s-s3-creds" (include "quill.fullname" $) $name) }}
                  key: AWS_ACCESS_KEY_ID
            - name: AWS_SECRET_ACCESS_KEY
              valueFrom:
                secretKeyRef:
                  name: {{ $b.auth.secret.existingSecret | default (printf "%s-%s-s3-creds" (include "quill.fullname" $) $name) }}
                  key: AWS_SECRET_ACCESS_KEY
            {{- end }}
          volumeMounts:
            - name: workdir
              mountPath: /workdir
          resources:
            {{- toYaml $b.resources | nindent 12 }}
      {{- end }}
      containers:
        - name: quill
          image: "{{ $inst.image.repository }}:{{ $inst.image.tag | default $.Chart.AppVersion }}"
          imagePullPolicy: {{ $inst.image.pullPolicy }}
          args: ["/etc/quill/config.toml"]
          ports:
            - name: teams
              containerPort: 3978
            - name: api
              containerPort: 8080
          envFrom:
            - secretRef:
                name: {{ include "quill.fullname" $ }}-{{ $name }}-secrets
          env:
            - name: QUILL_LOG
              value: {{ $inst.agent.logLevel | quote }}
          volumeMounts:
            - name: config
              mountPath: /etc/quill
              readOnly: true
            - name: workdir
              mountPath: {{ $inst.agent.workingDir }}
          livenessProbe:
            httpGet:
              path: /api/health
              port: api
            initialDelaySeconds: 10
            periodSeconds: 30
          readinessProbe:
            httpGet:
              path: /api/health
              port: api
            initialDelaySeconds: 5
            periodSeconds: 10
          {{- with $inst.resources }}
          resources:
            {{- toYaml . | nindent 12 }}
          {{- end }}
      volumes:
        - name: config
          configMap:
            name: {{ include "quill.fullname" $ }}-{{ $name }}-config
        - name: workdir
          emptyDir: {}
{{- end }}
{{- end }}
```

- [ ] **Step 6: Run the harness — scenarios 1 and 2 must both pass**

```
bash deploy/helm/quill/tests/template-tests.sh
```

Expected:

```
OK  scenario 1: default values produce no s3-sync sidecar
OK  scenario 2: IRSA mode renders sidecar + SA + IRSA annotation

All scenarios passed.
```

- [ ] **Step 7: Lint and commit**

```
helm lint deploy/helm/quill
```

Expected: 0 chart(s) failed.

```bash
git add deploy/helm/quill/templates/serviceaccount.yaml \
        deploy/helm/quill/templates/deployment.yaml \
        deploy/helm/quill/tests/values-irsa.yaml \
        deploy/helm/quill/tests/template-tests.sh
git commit -m "feat(helm): add S3 backup sidecar with IRSA auth"
```

---

## Task 3: EKS Pod Identity auth mode

**Goal:** Cover the `auth.mode=podIdentity` path. The ServiceAccount renders **without** the IRSA annotation; the pod still references it (so the EKS Pod Identity Agent can map it to a role via the out-of-band `PodIdentityAssociation`).

**Files:**
- Create: `deploy/helm/quill/tests/values-pod-identity.yaml`
- Modify: `deploy/helm/quill/tests/template-tests.sh`

(No template changes — the existing IRSA conditional `{{- if eq $b.auth.mode "irsa" }}` already gates the annotation correctly. This task is purely a test addition that proves the negative case.)

- [ ] **Step 1: Add the Pod Identity test fixture**

Create `deploy/helm/quill/tests/values-pod-identity.yaml`:

```yaml
backup:
  enabled: true
  s3:
    bucket: my-quill-backups
    prefix: prod/quill
    region: us-east-1
  auth:
    mode: podIdentity
```

- [ ] **Step 2: Append the Pod Identity scenario to the test harness**

In `deploy/helm/quill/tests/template-tests.sh`, append before the final "All scenarios passed." line:

```bash
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
```

- [ ] **Step 3: Run the harness — all three scenarios pass without code changes**

```
bash deploy/helm/quill/tests/template-tests.sh
```

Expected:

```
OK  scenario 1: default values produce no s3-sync sidecar
OK  scenario 2: IRSA mode renders sidecar + SA + IRSA annotation
OK  scenario 3: Pod Identity mode renders sidecar + bare SA

All scenarios passed.
```

- [ ] **Step 4: Lint and commit**

```
helm lint deploy/helm/quill
```

Expected: 0 chart(s) failed.

```bash
git add deploy/helm/quill/tests/values-pod-identity.yaml \
        deploy/helm/quill/tests/template-tests.sh
git commit -m "test(helm): cover Pod Identity auth mode"
```

---

## Task 4: Static-credential auth mode (inline + existingSecret)

**Goal:** Render the per-instance `Secret` when the user supplies inline credentials, and skip it when they point at an existing Secret. In both cases the sidecar's env block uses `valueFrom` to inject `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`.

**Files:**
- Create: `deploy/helm/quill/templates/secret-s3.yaml`
- Create: `deploy/helm/quill/tests/values-secret-inline.yaml`
- Create: `deploy/helm/quill/tests/values-secret-existing.yaml`
- Modify: `deploy/helm/quill/tests/template-tests.sh`

- [ ] **Step 1: Add the inline-secret test fixture**

Create `deploy/helm/quill/tests/values-secret-inline.yaml`:

```yaml
backup:
  enabled: true
  s3:
    bucket: my-quill-backups
    prefix: prod/quill
    region: us-east-1
  auth:
    mode: secret
    secret:
      accessKeyID: AKIAEXAMPLEEXAMPLE12
      secretAccessKey: deadbeefcafebabe1234567890abcdef
```

- [ ] **Step 2: Add the existingSecret test fixture**

Create `deploy/helm/quill/tests/values-secret-existing.yaml`:

```yaml
backup:
  enabled: true
  s3:
    bucket: my-quill-backups
    prefix: prod/quill
    region: us-east-1
  auth:
    mode: secret
    secret:
      existingSecret: my-precreated-aws-creds
```

- [ ] **Step 3: Append scenarios 4 and 5 to the test harness**

In `deploy/helm/quill/tests/template-tests.sh`, append before the final "All scenarios passed." line:

```bash
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
```

- [ ] **Step 4: Run the harness — scenarios 4 and 5 must FAIL**

```
bash deploy/helm/quill/tests/template-tests.sh
```

Expected: pass for scenarios 1, 2, 3; FAIL for scenario 4 — `chart-managed Secret r-quill-kiro-s3-creds not rendered`. The Secret template doesn't exist yet.

- [ ] **Step 5: Create `templates/secret-s3.yaml`**

```yaml
{{- range $name, $inst := .Values.instances }}
{{- if $inst.enabled }}
{{- $b := merge (default (dict) $inst.backup) $.Values.backup }}
{{- if and $b.enabled (eq $b.auth.mode "secret") (not $b.auth.secret.existingSecret) }}
---
apiVersion: v1
kind: Secret
metadata:
  name: {{ include "quill.fullname" $ }}-{{ $name }}-s3-creds
  labels:
    {{- include "quill.labels" $ | nindent 4 }}
    app.kubernetes.io/component: {{ $name }}
type: Opaque
data:
  AWS_ACCESS_KEY_ID: {{ $b.auth.secret.accessKeyID | b64enc }}
  AWS_SECRET_ACCESS_KEY: {{ $b.auth.secret.secretAccessKey | b64enc }}
{{- end }}
{{- end }}
{{- end }}
```

- [ ] **Step 6: Re-run the harness — all five scenarios pass**

```
bash deploy/helm/quill/tests/template-tests.sh
```

Expected:

```
OK  scenario 1: default values produce no s3-sync sidecar
OK  scenario 2: IRSA mode renders sidecar + SA + IRSA annotation
OK  scenario 3: Pod Identity mode renders sidecar + bare SA
OK  scenario 4: secret-inline mode renders sidecar + chart-managed Secret
OK  scenario 5: secret-existing mode references pre-existing Secret only

All scenarios passed.
```

- [ ] **Step 7: Lint and commit**

```
helm lint deploy/helm/quill
```

Expected: 0 chart(s) failed.

```bash
git add deploy/helm/quill/templates/secret-s3.yaml \
        deploy/helm/quill/tests/values-secret-inline.yaml \
        deploy/helm/quill/tests/values-secret-existing.yaml \
        deploy/helm/quill/tests/template-tests.sh
git commit -m "feat(helm): add static-credential auth mode for S3 backup"
```

---

## Task 5: README documentation

**Goal:** Add a "Session backup" section to the chart README that walks through the K8s 1.28+ requirement, the three auth modes (with copy-pasteable values blocks), the `replicas: 1` constraint, and how to set bucket retention via S3 lifecycle policies.

**Files:**
- Modify: `deploy/helm/quill/README.md`

- [ ] **Step 1: Append the Session backup section**

Append the following to `deploy/helm/quill/README.md` (after the "Multi-platform" section):

```markdown

## Session backup (S3)

Each instance's `agent.workingDir` (`/home/agent` for kiro, `/home/node` for the
Node-based agents) holds the agent's session state on disk:

- Kiro: `~/.kiro/sessions/cli/<uuid>.json`
- Claude: `~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`
- Codex: `~/.codex/history.jsonl`
- Copilot: `~/.copilot/session-state/<uuid>/...`

By default, `agent.workingDir` is an `emptyDir` volume — it is wiped on every
pod restart, so `/pick` returns nothing and users lose the ability to resume
prior conversations across image upgrades.

The chart can opt-in to an S3-backed backup that:

1. **Restores** the directory from `s3://{bucket}/{prefix}/{instance-name}` on
   pod start (via a Kubernetes 1.28+ native sidecar that runs `rclone copy`).
2. **Syncs back** to the same path on graceful pod termination (via the
   sidecar's `preStop` hook running `rclone sync`).

### Requirements

- **Kubernetes ≥ 1.28** — native sidecar containers (`restartPolicy: Always`
  inside `initContainers`) are required for the preStop ordering guarantee.
- **`replicas: 1`** — the chart hard-codes this. The backup design assumes a
  single writer; running multiple replicas risks race conditions on
  `rclone sync`. Do not edit `templates/deployment.yaml` to scale up.
- **Hard-crash trade-off** — OOMKill, node failure, or any abrupt termination
  bypasses `preStop`. State written since the last successful preStop is
  lost. Mitigate by sizing pod resources to avoid OOM and running on stable
  nodes.

### IRSA (recommended for EKS)

```yaml
backup:
  enabled: true
  s3:
    bucket: my-quill-backups
    prefix: prod/quill
    region: us-east-1
  auth:
    mode: irsa
    irsa:
      roleArn: arn:aws:iam::123456789012:role/quill-s3-backup
```

The IAM role must allow `s3:GetObject*`, `s3:PutObject*`, `s3:DeleteObject*`,
and `s3:ListBucket` against `s3://{bucket}/{prefix}/*`.

### EKS Pod Identity

```yaml
backup:
  enabled: true
  s3:
    bucket: my-quill-backups
    prefix: prod/quill
    region: us-east-1
  auth:
    mode: podIdentity
```

The chart renders a plain ServiceAccount; create the
`PodIdentityAssociation` separately (Terraform / eksctl / AWS console) tying
the ServiceAccount to the IAM role.

### Static credentials (dev / non-EKS)

```yaml
backup:
  enabled: true
  s3:
    bucket: my-quill-backups
    prefix: prod/quill
    region: us-east-1
  auth:
    mode: secret
    secret:
      accessKeyID: AKIA...
      secretAccessKey: ...
      # OR reference a pre-existing Secret with AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY keys:
      # existingSecret: my-aws-creds
```

### Retention

The chart does not manage retention — set an S3 lifecycle policy on the
bucket / prefix to age objects out:

```bash
aws s3api put-bucket-lifecycle-configuration \
  --bucket my-quill-backups \
  --lifecycle-configuration file://lifecycle.json
```

Where `lifecycle.json` expires objects under `prod/quill/` after, say, 90 days.

### Non-AWS S3 (MinIO, Cloudflare R2)

Set `backup.s3.endpoint` to override the default AWS endpoint. Use static
credentials and adjust `region` per the provider's documentation:

```yaml
backup:
  s3:
    bucket: quill
    prefix: dev/quill
    region: auto
    endpoint: https://<account-id>.r2.cloudflarestorage.com
  auth:
    mode: secret
    secret:
      accessKeyID: ...
      secretAccessKey: ...
```
```

- [ ] **Step 2: Commit**

```bash
git add deploy/helm/quill/README.md
git commit -m "docs(helm): document S3 backup auth modes and constraints"
```

---

## Task 6: Final integration sweep, push, and PR

**Goal:** Cross-cutting verification, tick automated DoD checkboxes in the spec, push branch, open PR.

**Files:**
- Verify: `deploy/helm/quill/` (all)
- Modify: `docs/superpowers/specs/2026-04-27-helm-s3-backup-design.md` (DoD checkboxes)

- [ ] **Step 1: Run the full template-test harness**

```
bash deploy/helm/quill/tests/template-tests.sh
```

Expected: all 5 scenarios pass.

- [ ] **Step 2: Run `helm lint` once more for the whole chart**

```
helm lint deploy/helm/quill
```

Expected: `1 chart(s) linted, 0 chart(s) failed`.

- [ ] **Step 3: Sanity-render IRSA mode and confirm YAML is structurally valid**

```
helm template r deploy/helm/quill -f deploy/helm/quill/tests/values-irsa.yaml \
  | python3 -c 'import sys, yaml; list(yaml.safe_load_all(sys.stdin))' \
  && echo OK
```

Expected: `OK`. (Splits and parses every `---`-separated document; will exit non-zero on any YAML error.)

- [ ] **Step 4: Tick the automated DoD checkboxes in the spec**

Edit `docs/superpowers/specs/2026-04-27-helm-s3-backup-design.md` Definition of Done section:

```markdown
- [x] `helm template` produces correct output for all four auth-mode scenarios.
- [x] `helm lint deploy/helm/quill` clean.
- [x] Chart `version` bumped (semver minor); `kubeVersion` reflects 1.28+.
- [x] `deploy/helm/quill/README.md` documents the feature, K8s constraint, and the three auth modes.
- [ ] Manual EKS verification: full restart cycle preserves `/pick` history.
- [ ] Backup disabled by default; existing deployments unaffected after `helm upgrade`.
```

(Keep the final two unchecked — they require a real EKS cluster.)

- [ ] **Step 5: Commit the spec update**

```bash
git add docs/superpowers/specs/2026-04-27-helm-s3-backup-design.md
git commit -m "docs(specs): tick automated DoD checkboxes for helm S3 backup"
```

- [ ] **Step 6: Push and open PR**

```bash
git push -u origin feat/helm-s3-backup
gh pr create --title "feat(helm): opt-in S3 backup for agent working directory" --body-file - <<'EOF'
##### Summary

- New `backup.*` block in `values.yaml` (disabled by default) plus a Kubernetes 1.28+ native sidecar that runs `rclone copy` on startup and `rclone sync` on graceful shutdown — preserving `/pick` history across pod restarts.
- Three AWS auth modes via `auth.mode`: `irsa` (renders ServiceAccount with `eks.amazonaws.com/role-arn`), `podIdentity` (plain ServiceAccount, association created out-of-band), `secret` (inline credentials in a chart-managed Secret, or `existingSecret` reference).
- Chart `version` bumped 0.1.0 → 0.2.0 and `kubeVersion: ">=1.28.0-0"` advertises the native-sidecar requirement.
- Hermetic test harness in `deploy/helm/quill/tests/` covers all five scenarios via `helm template` + `grep`.

##### Test plan

- [x] `bash deploy/helm/quill/tests/template-tests.sh` passes all 5 scenarios.
- [x] `helm lint deploy/helm/quill` clean.
- [x] `helm template ... | python3 yaml.safe_load_all` parses cleanly for the IRSA fixture.
- [ ] Manual EKS smoke: provision an IRSA role with S3 access, `helm upgrade --set backup.enabled=true`, send a few messages, `kubectl rollout restart`, confirm `/pick` still lists prior sessions.
- [ ] Verify `helm upgrade` on an existing deployment with `backup.enabled=false` produces no diff in the rendered Deployment / ServiceAccount / Secret manifests.

Spec: [`docs/superpowers/specs/2026-04-27-helm-s3-backup-design.md`](docs/superpowers/specs/2026-04-27-helm-s3-backup-design.md)
Plan: [`docs/superpowers/plans/2026-04-27-helm-s3-backup.md`](docs/superpowers/plans/2026-04-27-helm-s3-backup.md)
EOF
```

Report the PR URL when done.

---

## Self-Review Notes (for the implementer)

These are the observations I made writing the plan. Knowing them up front saves debugging time.

1. **`merge` semantics** — Helm's `merge` mutates the *first* argument and returns it. For per-instance overrides via `merge (default (dict) $inst.backup) $.Values.backup`, the keys defined in `$inst.backup` win over the global default. If you ever need the *opposite* precedence (global wins), swap the argument order.

2. **Native sidecar ordering** — On K8s 1.28+, native sidecars (`restartPolicy: Always` inside `initContainers`) start *before* the main container and shut down *after* it. The preStop hook on the sidecar fires *after* the main container has exited. This is the property that makes the design correct — the main container has finished writing to `/workdir` before the sync runs. Do not change `restartPolicy` or move the sidecar to `containers:`.

3. **`|| true` on restore** — The restore command swallows errors so a misconfigured IAM role / empty bucket does not block the main container from starting. The pod still surfaces the rclone failure in logs. We deliberately do NOT do this on the preStop sync — there, an error should be visible in pod events so operators can investigate.

4. **`grep` on multi-line YAML** — The test harness uses `grep -q 'pattern'` for simplicity. For tighter assertions you would reach for `yq`, but Helm's output ordering is stable enough that string-grep over the whole rendered document is sufficient and avoids a `yq` dependency.

5. **`r-quill-kiro` in tests** — Tests render with release name `r` (single letter, by convention `helm template r ...`). The full ServiceAccount / Secret names compose to `r-quill-kiro-...`, hence the literal `r-quill-kiro-s3-creds` and `serviceAccountName: r-quill-kiro` substrings in assertions.

6. **`yaml.safe_load_all`** — Step 3 of Task 6 uses Python because `helm lint` does not validate cross-document references. `safe_load_all` parses each YAML document and surfaces any structural error (mis-indented values, broken templates).

7. **`backup.enabled` is intentionally additive** — Existing deployments upgrading to chart `0.2.0` see no change because all backup defaults are inert until `enabled: true` is explicitly set. The README documents this so ops can `helm upgrade --reuse-values` confidently.
