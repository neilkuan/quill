# Helm S3 Restore Race + Ownership Fix — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix two compounding defects in the S3 backup feature so pod restart actually restores `/home/agent` from S3 with files owned 1000:1000 — usable by the quill agent process.

**Architecture:** Replace the single race-prone `s3-sync` sidecar with two cooperating containers sharing the existing `workdir` emptyDir: a plain `s3-restore` initContainer (must exit before main is admitted) and a trimmed-down `s3-backup` native sidecar (runs `preStop` `rclone sync`). Both run as UID:GID 1000:1000 and the pod sets `fsGroup: 1000` so emptyDir contents are agent-readable.

**Tech Stack:** Helm 3 templates, Kubernetes 1.28+ native sidecars, `rclone/rclone:1.66`, bash test harness with `helm template` + grep, kubelet `securityContext`/`fsGroup` semantics.

**Spec:** `docs/superpowers/specs/2026-04-29-helm-s3-restore-race-fix-design.md`

---

## File Map

- Modify `deploy/helm/quill/values.yaml` — add `backup.ownership` block with 1000:1000 defaults
- Modify `deploy/helm/quill/templates/_helpers.tpl` — add `quill.s3.envVars` named template
- Modify `deploy/helm/quill/templates/deployment.yaml` — split sidecar, add securityContext, add fsGroup
- Modify `deploy/helm/quill/Chart.yaml` — bump chart `version`
- Modify `deploy/helm/quill/tests/template-tests.sh` — rename `s3-sync` references, add ownership/fsGroup/HOME assertions, add custom-uid scenario
- Create `deploy/helm/quill/tests/values-custom-uid.yaml` — fixture for custom UID/GID override
- Modify `deploy/helm/quill/README.md` — note ownership requirement and restore-race fix
- Touch `VERSION` only if releasing — out of scope here; PR will be merged before next release cuts

---

## Task 0: Pre-flight — confirm baseline

**Files:** none (verification only)

- [ ] **Step 1: Confirm we're on the right branch**

Run: `git branch --show-current`
Expected: `fix/helm-s3-restore-race`

If on a different branch, stop and switch. The branch was created during brainstorming.

- [ ] **Step 2: Run existing chart tests against the un-modified chart**

Run: `bash deploy/helm/quill/tests/template-tests.sh`
Expected: all five scenarios print `OK`, ending with `All scenarios passed.`

This confirms the baseline. Any failure here is unrelated to our work and must be triaged before continuing.

- [ ] **Step 3: Confirm `helm` and `yq` are on PATH**

Run: `helm version --short && yq --version`
Expected: helm v3.x and a yq version string.

If `yq` is missing, install with `brew install yq` (project tests only need `grep` today, but new assertions will use `yq` for structural checks).

---

## Task 1: Add `backup.ownership` block to `values.yaml`

**Files:**
- Modify: `deploy/helm/quill/values.yaml:75-107` (the `backup:` block)

- [ ] **Step 1: Insert the `ownership` sub-block immediately after `rclone:` and before `resources:`**

Edit `deploy/helm/quill/values.yaml`. Find the `rclone:` block (line 96) and add `ownership:` after `extraArgs: []` and before `resources:`:

```yaml
  rclone:
    image: rclone/rclone
    tag: "1.66"
    pullPolicy: IfNotPresent
    extraArgs: []          # passed to both copy (restore) and sync (backup)
  ownership:
    # UID/GID rclone containers run as. Files restored from S3 are owned by these
    # IDs, so they must match the UID/GID inside the quill main container. All
    # four built-in agent images (kiro, claude, codex, copilot) run as 1000:1000.
    # Override only if you build a custom image with a different runtime user.
    runAsUser: 1000
    runAsGroup: 1000
    # Pod-level fsGroup applied to the shared emptyDir. Kubelet recursively chowns
    # volume contents to this GID at mount time and sets the SGID bit so files
    # created later inherit the GID. Set the same as runAsGroup unless you have a
    # specific reason to differ.
    fsGroup: 1000
  resources:
    requests:
      cpu: 10m
      memory: 32Mi
    limits:
      cpu: 100m
      memory: 64Mi
```

- [ ] **Step 2: Verify the YAML still parses by re-rendering**

Run: `helm template r deploy/helm/quill -f deploy/helm/quill/tests/values-irsa.yaml > /dev/null`
Expected: command exits 0 with no error output.

- [ ] **Step 3: Run existing tests — they must still pass**

Run: `bash deploy/helm/quill/tests/template-tests.sh`
Expected: all five scenarios `OK`. Adding values keys without using them yet does not affect rendered output.

- [ ] **Step 4: Commit**

```bash
git add deploy/helm/quill/values.yaml
git commit -m "feat(helm): add backup.ownership defaults (1000:1000)"
```

---

## Task 2: Extract rclone env vars to `quill.s3.envVars` helper (refactor)

This is a pure refactor: same rendered output, but env vars now come from a single named template. Lets us avoid duplicating the env block when we split the sidecar into two containers.

**Files:**
- Modify: `deploy/helm/quill/templates/_helpers.tpl` (append the new helper)
- Modify: `deploy/helm/quill/templates/deployment.yaml:57-87` (replace inline env block with helper invocation)

- [ ] **Step 1: Append `quill.s3.envVars` definition to `_helpers.tpl`**

Add to the bottom of `deploy/helm/quill/templates/_helpers.tpl`:

```gotmpl
{{/*
S3 sync env vars shared by both rclone init/sidecar containers.
Caller dict:
  ctx  — the root context (`$`) for fullname helper
  name — the instance name (e.g. "kiro")
  b    — the merged backup config (`merge $inst.backup $.Values.backup`)
*/}}
{{- define "quill.s3.envVars" -}}
- name: HOME
  value: /tmp
- name: BUCKET
  value: {{ .b.s3.bucket | quote }}
- name: PREFIX
  value: "{{ .b.s3.prefix }}/{{ .name }}"
- name: RCLONE_CONFIG_S3_TYPE
  value: s3
- name: RCLONE_CONFIG_S3_PROVIDER
  value: AWS
- name: RCLONE_CONFIG_S3_REGION
  value: {{ .b.s3.region | quote }}
- name: RCLONE_CONFIG_S3_ENV_AUTH
  value: "true"
{{- if .b.s3.endpoint }}
- name: RCLONE_CONFIG_S3_ENDPOINT
  value: {{ .b.s3.endpoint | quote }}
{{- end }}
- name: EXTRA_ARGS
  value: {{ join " " .b.rclone.extraArgs | quote }}
{{- if eq .b.auth.mode "secret" }}
- name: AWS_ACCESS_KEY_ID
  valueFrom:
    secretKeyRef:
      name: {{ .b.auth.secret.existingSecret | default (printf "%s-%s-s3-creds" (include "quill.fullname" .ctx) .name) }}
      key: AWS_ACCESS_KEY_ID
- name: AWS_SECRET_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .b.auth.secret.existingSecret | default (printf "%s-%s-s3-creds" (include "quill.fullname" .ctx) .name) }}
      key: AWS_SECRET_ACCESS_KEY
{{- end }}
{{- end -}}
```

Note: `HOME=/tmp` is included here so both rclone containers inherit it (rclone tries to write `~/.cache/rclone/`; UID 1000 has no `/etc/passwd` entry in the rclone image so HOME would otherwise resolve to the read-only `/`).

- [ ] **Step 2: Replace the inline `env:` list in `deployment.yaml` with the helper invocation**

Find the existing `env:` block on the `s3-sync` initContainer (lines 57–87 of `deploy/helm/quill/templates/deployment.yaml`):

```yaml
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
```

Replace with:

```yaml
          env:
            {{- include "quill.s3.envVars" (dict "ctx" $ "name" $name "b" $b) | nindent 12 }}
```

- [ ] **Step 3: Render and diff to confirm output is byte-identical**

Run: `helm template r deploy/helm/quill -f deploy/helm/quill/tests/values-irsa.yaml | grep -A 25 'env:' | head -40`
Expected: same env-var list as before (BUCKET, PREFIX, RCLONE_*, EXTRA_ARGS) — the only **new** entry is `HOME` at the top, which is the intentional addition.

If anything else differs in ordering, the helper has a bug — fix and rerun.

- [ ] **Step 4: Run all template tests — pass with the addition of HOME**

Run: `bash deploy/helm/quill/tests/template-tests.sh`
Expected: all five scenarios `OK`. (The existing greps for `BUCKET`, `PREFIX`, `AWS_ACCESS_KEY_ID` still match the helper's output.)

- [ ] **Step 5: Commit**

```bash
git add deploy/helm/quill/templates/_helpers.tpl deploy/helm/quill/templates/deployment.yaml
git commit -m "refactor(helm): extract rclone env vars to quill.s3.envVars helper"
```

---

## Task 3: Update test assertions for new container names + add ownership/fsGroup checks (failing tests first)

This task writes the new tests **without** modifying the deployment template — so the suite must FAIL after this task and PASS after Task 4.

**Files:**
- Modify: `deploy/helm/quill/tests/template-tests.sh` (replace `s3-sync` references with two-container assertions, add `runAsUser`/`fsGroup`/`HOME` greps)

- [ ] **Step 1: Rewrite scenario 1 (disabled path) to assert neither new container exists**

Replace lines 25–29 of `template-tests.sh`:

```bash
# Scenario 1: backup disabled (default values) — no init/sidecar
render "default" ""
grep -q 'name: s3-restore' "$TMP/default.yaml" \
    && fail "scenario 1: s3-restore should NOT render when backup is disabled"
grep -q 'name: s3-backup' "$TMP/default.yaml" \
    && fail "scenario 1: s3-backup should NOT render when backup is disabled"
grep -q 'fsGroup:' "$TMP/default.yaml" \
    && fail "scenario 1: pod-level fsGroup should not appear when backup is disabled"
pass "scenario 1: default values produce no rclone containers and no fsGroup"
```

- [ ] **Step 2: Rewrite scenario 2 (IRSA) to assert two containers + ownership + fsGroup + HOME**

Replace lines 31–61 of `template-tests.sh`:

```bash
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
```

- [ ] **Step 3: Update scenarios 3-5 to use the new container names**

In scenarios 3, 4, 5 (lines 64–109), replace each `grep -q 'name: s3-sync'` with two consecutive checks:

```bash
grep -q 'name: s3-restore' "$TMP/<file>.yaml" \
    || fail "scenario N: s3-restore initContainer missing"
grep -q 'name: s3-backup' "$TMP/<file>.yaml" \
    || fail "scenario N: s3-backup sidecar missing"
```

Apply this find-and-replace logic to scenarios 3 (`pod-identity`), 4 (`secret-inline`), and 5 (`secret-existing`). Other assertions in those scenarios (Secret rendering, ServiceAccount presence/absence) stay unchanged.

- [ ] **Step 4: Run the test script — expect failure**

Run: `bash deploy/helm/quill/tests/template-tests.sh`
Expected: scenario 1 may pass (since deployment.yaml currently has `name: s3-sync` only and `s3-restore`/`s3-backup` don't appear) — but scenario 2 will FAIL on the first new assertion (`s3-restore initContainer missing`). The whole script halts on first failure.

This is the desired state — tests describing new behavior, awaiting the template change.

- [ ] **Step 5: Commit the failing tests**

```bash
git add deploy/helm/quill/tests/template-tests.sh
git commit -m "test(helm): assert two-container layout + 1000:1000 ownership"
```

---

## Task 4: Replace single sidecar with `s3-restore` + `s3-backup` in `deployment.yaml`

This is the meat of the fix. The template change makes Task 3's failing tests pass.

**Files:**
- Modify: `deploy/helm/quill/templates/deployment.yaml:30-93` (the entire current `initContainers` block + add pod-level securityContext)

- [ ] **Step 1: Add pod-level `securityContext` (only when backup enabled)**

In `deploy/helm/quill/templates/deployment.yaml`, after `terminationGracePeriodSeconds: 60` (line 31) and before the `{{- if and $b.enabled ... }}` block for `serviceAccountName` (line 32), insert:

```yaml
      terminationGracePeriodSeconds: 60
      {{- if $b.enabled }}
      securityContext:
        fsGroup: {{ $b.ownership.fsGroup }}
      {{- end }}
      {{- if and $b.enabled (or (eq $b.auth.mode "irsa") (eq $b.auth.mode "podIdentity")) }}
      serviceAccountName: {{ include "quill.fullname" $ }}-{{ $name }}
      {{- end }}
```

- [ ] **Step 2: Replace the entire `initContainers` block with the two-container layout**

Replace lines 35–93 (the `{{- if $b.enabled }}` … `{{- end }}` block currently containing the single `s3-sync` container) with:

```yaml
      {{- if $b.enabled }}
      initContainers:
        - name: s3-restore
          image: "{{ $b.rclone.image }}:{{ $b.rclone.tag }}"
          imagePullPolicy: {{ $b.rclone.pullPolicy }}
          securityContext:
            runAsUser: {{ $b.ownership.runAsUser }}
            runAsGroup: {{ $b.ownership.runAsGroup }}
            runAsNonRoot: true
          command:
            - /bin/sh
            - -c
            - |
              set -eu
              rclone copy "s3:${BUCKET}/${PREFIX}" /workdir --create-empty-src-dirs $EXTRA_ARGS
          env:
            {{- include "quill.s3.envVars" (dict "ctx" $ "name" $name "b" $b) | nindent 12 }}
          volumeMounts:
            - name: workdir
              mountPath: /workdir
          resources:
            {{- toYaml $b.resources | nindent 12 }}
        - name: s3-backup
          image: "{{ $b.rclone.image }}:{{ $b.rclone.tag }}"
          imagePullPolicy: {{ $b.rclone.pullPolicy }}
          restartPolicy: Always
          securityContext:
            runAsUser: {{ $b.ownership.runAsUser }}
            runAsGroup: {{ $b.ownership.runAsGroup }}
            runAsNonRoot: true
          command:
            - /bin/sh
            - -c
            - exec sleep infinity
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
            {{- include "quill.s3.envVars" (dict "ctx" $ "name" $name "b" $b) | nindent 12 }}
          volumeMounts:
            - name: workdir
              mountPath: /workdir
          resources:
            {{- toYaml $b.resources | nindent 12 }}
      {{- end }}
```

Note three deliberate changes vs the 2026-04-27 design:

1. `s3-restore` has **no** `|| true` — a failed restore CrashLoops the pod loudly. Empty S3 prefix returns 0 (no files is not an error).
2. `s3-backup` uses `exec sleep infinity` instead of `while true; do sleep 3600; done` — `exec` makes `sleep` PID 1 inside the container so SIGTERM is delivered cleanly, and `sleep infinity` (BusyBox supports this) terminates immediately on signal.
3. Both containers carry `securityContext.runAsUser/runAsGroup/runAsNonRoot` — files written into the shared emptyDir end up owned by 1000:1000.

- [ ] **Step 3: Render once and visually diff against the prior version**

Run: `helm template r deploy/helm/quill -f deploy/helm/quill/tests/values-irsa.yaml | grep -A 50 'initContainers:'`
Expected: two containers (`s3-restore` then `s3-backup`), each with `securityContext`, `runAsUser: 1000`, `runAsGroup: 1000`, `runAsNonRoot: true`. The pod also has `securityContext.fsGroup: 1000` at the `spec.template.spec` level.

- [ ] **Step 4: Run all template tests — must now pass**

Run: `bash deploy/helm/quill/tests/template-tests.sh`
Expected: all five scenarios `OK`, ending with `All scenarios passed.`

If a scenario fails, read the failure message — typical issues:
- `yq` produced `null` instead of the expected value: indent or context-binding bug in the helper or the template.
- `securityContext` indentation wrong: pod-level vs container-level got merged. The pod-level block is at `spec.template.spec`, not at `spec`.

- [ ] **Step 5: Commit**

```bash
git add deploy/helm/quill/templates/deployment.yaml
git commit -m "fix(helm): split S3 sidecar to close restore race and own files 1000:1000

Replaces single race-prone s3-sync sidecar with a plain s3-restore
initContainer (must complete before main is admitted) plus a slimmed-down
s3-backup native sidecar that runs preStop rclone sync. Both rclone
containers run as UID:GID 1000:1000 with runAsNonRoot, and the pod
declares fsGroup: 1000 on the shared emptyDir so files restored from S3
are read/writable by the agent process."
```

---

## Task 5: Add `tests/values-custom-uid.yaml` fixture + scenario 6

Drives the ownership values through, proves they're not hard-coded.

**Files:**
- Create: `deploy/helm/quill/tests/values-custom-uid.yaml`
- Modify: `deploy/helm/quill/tests/template-tests.sh` (append scenario 6)

- [ ] **Step 1: Create the fixture**

Write `deploy/helm/quill/tests/values-custom-uid.yaml`:

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
  ownership:
    runAsUser: 2000
    runAsGroup: 3000
    fsGroup: 3000
```

- [ ] **Step 2: Append scenario 6 to `template-tests.sh`**

Just before the final `echo` and `echo "All scenarios passed."` lines, insert:

```bash
# Scenario 6: custom ownership — values override defaults
render "custom-uid" "$TESTS_DIR/values-custom-uid.yaml"
yq 'select(.kind == "Deployment").spec.template.spec.securityContext.fsGroup' "$TMP/custom-uid.yaml" | grep -q '^3000$' \
    || fail "scenario 6: pod fsGroup should be 3000"
for c in s3-restore s3-backup; do
    uid=$(yq "select(.kind == \"Deployment\").spec.template.spec.initContainers[] | select(.name == \"$c\").securityContext.runAsUser" "$TMP/custom-uid.yaml")
    gid=$(yq "select(.kind == \"Deployment\").spec.template.spec.initContainers[] | select(.name == \"$c\").securityContext.runAsGroup" "$TMP/custom-uid.yaml")
    [[ "$uid" == "2000" ]] || fail "scenario 6: $c runAsUser should be 2000, got $uid"
    [[ "$gid" == "3000" ]] || fail "scenario 6: $c runAsGroup should be 3000, got $gid"
done
pass "scenario 6: custom ownership values propagate to securityContext + fsGroup"
```

- [ ] **Step 3: Run the test suite — all six scenarios pass**

Run: `bash deploy/helm/quill/tests/template-tests.sh`
Expected: scenarios 1–6 all `OK`, ending with `All scenarios passed.`

- [ ] **Step 4: Commit**

```bash
git add deploy/helm/quill/tests/values-custom-uid.yaml deploy/helm/quill/tests/template-tests.sh
git commit -m "test(helm): cover custom backup.ownership UID/GID overrides"
```

---

## Task 6: Bump chart version + README note

**Files:**
- Modify: `deploy/helm/quill/Chart.yaml:5` (bump `version`)
- Modify: `deploy/helm/quill/README.md` (insert a note in the "Session backup (S3)" section)

- [ ] **Step 1: Bump `Chart.yaml` version**

Edit `deploy/helm/quill/Chart.yaml`:

```yaml
apiVersion: v2
name: quill
description: ACP bridge for Discord, Telegram, and Microsoft Teams
type: application
version: 0.3.0
appVersion: ""
kubeVersion: ">=1.28.0-0"
```

(Bump `0.2.0` → `0.3.0` — semver minor: existing schema preserved, new optional `backup.ownership` block added, fixed visible behaviour bug.)

- [ ] **Step 2: Update `deploy/helm/quill/README.md` — add a "File ownership" sub-section under "Session backup (S3)"**

Insert this block immediately after the existing `### Requirements` section in `deploy/helm/quill/README.md` (around line 102, before `### IRSA (recommended for EKS)`):

```markdown
### File ownership

The two rclone containers (`s3-restore` initContainer and `s3-backup` native
sidecar) run as **UID:GID 1000:1000** by default — matching the runtime user
in every built-in agent image (`agent` for kiro, `node` for claude/codex/copilot,
both UID 1000). The pod also sets `securityContext.fsGroup: 1000` so the
shared `emptyDir` is owned by GID 1000.

This is what makes restore actually work: files arriving from S3 are owned
by the same user the quill agent process runs as, so the agent can read
them and write back into the same directory tree (e.g., SQLite locks on
`data.sqlite3`).

If you build a custom agent image that runs as a different user, override
the defaults:

```yaml
backup:
  enabled: true
  ownership:
    runAsUser: 2000
    runAsGroup: 2000
    fsGroup: 2000
```
```

- [ ] **Step 3: Run all tests one last time as a sanity check**

Run: `bash deploy/helm/quill/tests/template-tests.sh`
Expected: all six scenarios `OK`.

- [ ] **Step 4: Commit**

```bash
git add deploy/helm/quill/Chart.yaml deploy/helm/quill/README.md
git commit -m "docs(helm): document file ownership and bump chart to 0.3.0"
```

---

## Task 7: Manual smoke test (post-merge, in a real cluster)

Not automatable in CI — needs a live EKS cluster with IRSA configured. Document and execute before declaring the PR done.

**Files:** none (verification on a live cluster)

- [ ] **Step 1: Deploy to a dev namespace**

```bash
helm upgrade --install quill-dev deploy/helm/quill \
  -n quill-dev --create-namespace \
  -f path/to/your/dev-values.yaml \
  --set backup.enabled=true \
  --set backup.s3.bucket=aws-tperd-splashtop-premium-tool-ap-northeast-1 \
  --set backup.s3.prefix=quill \
  --set backup.s3.region=ap-northeast-1 \
  --set backup.auth.mode=irsa \
  --set backup.auth.irsa.roleArn=arn:aws:iam::<acct>:role/quill-s3-backup
```

Confirm: `kubectl -n quill-dev get pods` shows the pod `Running` with both `s3-restore` (Completed) and `s3-backup` (Running) listed under `kubectl describe pod ...`.

- [ ] **Step 2: Verify file ownership in the running pod**

```bash
POD=$(kubectl -n quill-dev get pod -l app.kubernetes.io/component=kiro -o jsonpath='{.items[0].metadata.name}')
kubectl -n quill-dev exec "$POD" -c quill -- stat -c '%u:%g %n' \
    /home/agent/.local/share/kiro-cli/data.sqlite3 \
    /home/agent/.kiro \
    /home/agent
```

Expected: every line starts with `1000:1000 …`.

- [ ] **Step 3: Verify writability**

```bash
kubectl -n quill-dev exec "$POD" -c quill -- sh -c \
    'touch /home/agent/.write-probe && rm /home/agent/.write-probe && echo OK'
```

Expected: prints `OK`. Permission denied means fsGroup or runAsUser is wrong.

- [ ] **Step 4: Verify session listing**

From a chat client connected to the dev pod, send `/pick` (or `/sessions`).
Expected: returns the historical session list previously persisted to S3 — not an empty list.

- [ ] **Step 5: Verify preStop side still works**

Trigger a graceful pod restart:

```bash
kubectl -n quill-dev rollout restart deploy quill-dev-quill-kiro
```

While the old pod is terminating, watch its logs:

```bash
kubectl -n quill-dev logs "$POD" -c s3-backup --previous
```

Expected: see `rclone sync` log lines (file uploads) before the pod terminates.

After the rollout completes, repeat Step 4 — `/pick` should still return the session list, proving the round-trip works.

---

## Self-review

After implementing all tasks, review against the spec at `docs/superpowers/specs/2026-04-29-helm-s3-restore-race-fix-design.md`:

- Spec **Goals** → covered by Tasks 4 (race fix), 4+1 (ownership), 1 (values schema), 6 (Chart.yaml).
- Spec **Components → values.yaml ownership block** → Task 1.
- Spec **Components → deployment.yaml pod-level securityContext** → Task 4 step 1.
- Spec **Components → s3-restore + s3-backup containers** → Task 4 step 2.
- Spec **Components → quill.s3.envVars helper** → Task 2.
- Spec **Components → Chart.yaml version bump** → Task 6.
- Spec **Tests → 1-14** → Task 3 (scenarios 1-5 rewritten) + Task 5 (scenario 6 new fixture).
- Spec **Manual smoke test** → Task 7.

No placeholders, no "TBD", every step has either runnable code or an exact command + expected output.
