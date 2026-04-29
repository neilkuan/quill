# Helm Chart — Fix S3 Backup Restore Race on Pod Startup

**Date:** 2026-04-29
**Branch:** `release/v0.22.2` (working branch — actual implementation branch chosen by writing-plans)
**Status:** Spec — pending implementation plan
**Supersedes (partially):** `2026-04-27-helm-s3-backup-design.md` — Architecture section only. Goals, non-goals, values schema, auth modes are unchanged.

## Background

The S3 backup feature shipped in 2026-04-27 uses a single Kubernetes 1.28+
native sidecar (`initContainers` with `restartPolicy: Always`) to handle
both directions:

- **On startup**: `rclone copy s3://… /workdir`, then `while true; do sleep 3600; done`
- **On termination**: `lifecycle.preStop` runs `rclone sync /workdir s3://…`

Field reports show that S3 receives backups (preStop side works), but pods
restart with an empty `/home/agent` (restore side fails). Confirmed by:

```
$ aws s3 ls s3://aws-tperd-splashtop-premium-tool-ap-northeast-1 --recursive
2026-04-29 07:40:31         39 quill/kiro/.bash_history
2026-04-29 07:40:32         19 quill/kiro/.kiro/.cli_bash_history
2026-04-29 09:19:04      36864 quill/kiro/.local/share/kiro-cli/data.sqlite3
2026-04-29 07:40:32     466247 quill/kiro/.semantic_search/models/all-MiniLM-L6-v2/tokenizer.json
```

S3 has session data, but the Kiro agent reports an empty session list after
pod restart.

## Root cause

Native sidecar startup ordering only guarantees that the sidecar **process
has started** before the main container is admitted — it does **not** wait
for the sidecar's entrypoint command to complete. The current sidecar
script is:

```sh
rclone copy "s3:${BUCKET}/${PREFIX}" /workdir --create-empty-src-dirs $EXTRA_ARGS || true
while true; do sleep 3600; done
```

The kubelet sees the `sh` process running, marks the sidecar `Started`, and
admits the main container. Meanwhile, `rclone copy` is still streaming
files. The Kiro agent boots, scans `/home/agent`, finds it empty (or
partially populated), and reports no resumable sessions. Files arriving
later are correct on disk but already missed by the agent's startup scan.

Without a synchronous gate ("restore must finish before main starts"), this
race is unfixable inside a single sidecar definition.

## Goals

- Pod start → `/home/agent` is fully hydrated from S3 **before** the quill
  main container's process begins, every time.
- Pod terminates gracefully → unchanged from the 2026-04-27 design (preStop
  sync continues to work as today).
- No change to `values.yaml` schema, auth modes, Dockerfile variants, or
  the `replicas: 1` constraint.
- Disabled-by-default behaviour preserved (`backup.enabled=false` keeps the
  current single-emptyDir layout).

## Non-goals

- Periodic mid-flight sync — explicitly deferred to a follow-up; abrupt
  termination (OOMKill, node failure) still loses data back to last preStop.
- Aligning the sidecar's mountPath to per-instance `agent.workingDir`
  (`/home/agent` vs `/home/node`). Sidecar template stays at `/workdir`
  because the same template serves multiple instances; the underlying
  emptyDir volume is shared so file contents are identical across paths.
- Increasing `terminationGracePeriodSeconds`. Current 60s stays; revisit
  only if backup-side timeouts surface separately.
- Migrating to PVC, csi-s3, or other durable-volume approaches.

## Architecture

Replace the single sidecar with two cooperating containers sharing the
existing `workdir` emptyDir volume:

```
Pod created
  │
  ├─ initContainers (run in declaration order; each must exit before next)
  │  │
  │  ├─ s3-restore                     ← plain init container
  │  │   rclone copy s3://…/{instance} /workdir
  │  │   exits 0 → kubelet advances to next initContainer
  │  │
  │  └─ s3-backup (sidecar)            ← restartPolicy: Always
  │      exec sleep infinity
  │      preStop: rclone sync /workdir s3://…/{instance}
  │      kubelet sees process started → admits main container
  │
  └─ containers
     └─ quill                          ← /home/agent already populated
        agent boots, scans dir, sees full session history
```

Key shift: **restore moves out of the sidecar into a regular init
container**. Regular init containers must exit before the next container
in the spec is admitted, which gives us the synchronous gate the current
design lacks. The sidecar shrinks to "idle process that holds a preStop
hook" — its only job is to live long enough to receive SIGTERM after main
exits, so the preStop fires.

### Termination flow (unchanged from 2026-04-27)

1. Pod receives termination signal.
2. Main container `quill` receives SIGTERM, drains, exits.
3. Sidecar `s3-backup` receives SIGTERM (kubelet ordering: sidecars after
   main containers).
4. `lifecycle.preStop` hook executes `rclone sync` before the sidecar
   process is sent SIGTERM.
5. Sidecar exits, pod terminated.

This path was working in the 2026-04-27 implementation and we keep it
verbatim.

## Components

### `deploy/helm/quill/templates/deployment.yaml`

Replace the current single-`s3-sync` initContainer block with two entries.
Both gated by the same `{{ if $b.enabled }}`.

**Container 1: `s3-restore`** (no `restartPolicy` field — plain init)

```yaml
- name: s3-restore
  image: "{{ $b.rclone.image }}:{{ $b.rclone.tag }}"
  imagePullPolicy: {{ $b.rclone.pullPolicy }}
  command:
    - /bin/sh
    - -c
    - |
      set -eu
      rclone copy "s3:${BUCKET}/${PREFIX}" /workdir --create-empty-src-dirs $EXTRA_ARGS
  env:
    {{- include "quill.s3.envVars" (dict "ctx" $ "name" $name "b" $b) | nindent 4 }}
  volumeMounts:
    - name: workdir
      mountPath: /workdir
  resources:
    {{- toYaml $b.resources | nindent 4 }}
```

Note the removal of `|| true`. If S3 access is misconfigured (wrong bucket,
expired creds, missing IRSA), we want the pod to CrashLoop loudly rather
than silently start with an empty directory and proceed to overwrite the
S3 backup with nothing on the next preStop. First-time deploy with an
empty prefix is a non-issue: `rclone copy` against a non-existent prefix
returns 0 (no files to copy is not an error).

**Container 2: `s3-backup`** (sidecar — `restartPolicy: Always`)

```yaml
- name: s3-backup
  image: "{{ $b.rclone.image }}:{{ $b.rclone.tag }}"
  imagePullPolicy: {{ $b.rclone.pullPolicy }}
  restartPolicy: Always
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
    {{- include "quill.s3.envVars" (dict "ctx" $ "name" $name "b" $b) | nindent 4 }}
  volumeMounts:
    - name: workdir
      mountPath: /workdir
  resources:
    {{- toYaml $b.resources | nindent 4 }}
```

`exec sleep infinity` replaces `while true; do sleep 3600; done`:

- `exec` makes `sleep` PID 1, so SIGTERM is delivered cleanly.
- `sleep infinity` (BusyBox supports this) ends immediately on SIGTERM
  without needing to wait out a 3600s tick.
- preStop runs synchronously before the SIGTERM is sent, so this only
  matters if preStop hangs and grace period expires — but it does mean
  the SIGKILL fallback is responsive.

### New helper: `deploy/helm/quill/templates/_helpers.tpl`

Both rclone containers share identical env vars. Move the duplicated env
list into a named template:

```gotmpl
{{- define "quill.s3.envVars" -}}
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

Caller pattern: `{{- include "quill.s3.envVars" (dict "ctx" $ "name" $name "b" $b) | nindent 12 }}` (indent depth depends on caller's nesting level inside the YAML).

### `deploy/helm/quill/Chart.yaml`

Bump chart `version` (semver patch — bug fix to existing feature, no
schema change). `appVersion` unchanged.

### Unchanged

- `values.yaml` — `backup` block schema is identical.
- `templates/serviceaccount.yaml` — IRSA / podIdentity SA logic unchanged.
- `templates/secret-s3.yaml` — secret-mode credential generation unchanged.
- All other templates.

## Tests

### `tests/template-tests.sh` — new assertions

Existing tests (`values-irsa.yaml`, `values-pod-identity.yaml`,
`values-secret-existing.yaml`, `values-secret-inline.yaml`) all enable
backup. Extend the harness to assert:

1. **Two initContainers exist when backup enabled**
   `helm template . -f tests/values-irsa.yaml` produces a Deployment with
   exactly two `initContainers` entries: `s3-restore` then `s3-backup`.
2. **Order matters**: `s3-restore` appears first, `s3-backup` second.
3. **`s3-restore` is a plain init**: no `restartPolicy` field.
4. **`s3-backup` is a native sidecar**: `restartPolicy: Always`.
5. **`s3-restore` carries no `lifecycle.preStop`**.
6. **`s3-backup` carries `lifecycle.preStop`** with an `rclone sync …`
   command.
7. **`s3-restore` command contains `rclone copy`** but **not `sleep`**.
8. **`s3-backup` command contains `sleep infinity`** but **not `rclone copy`**.
9. **`backup.enabled: false` produces zero initContainers** (regression
   guard for the disabled path).
10. **All three auth modes still render** (existing assertions retained).

Implementation: extend the bash assertions in `tests/template-tests.sh`
using `yq` selectors against the rendered YAML.

### Manual smoke test (post-merge, in a real cluster)

Documented in the implementation plan, not the spec — but in scope:

1. Deploy with `backup.enabled=true`, IRSA mode.
2. Have agent generate session data, confirm preStop sync to S3.
3. Delete pod (`kubectl delete pod …`).
4. New pod starts → confirm `/home/agent` is fully populated **before**
   `quill` process emits its first log line (check kubelet event timeline).
5. `/pick` from a chat client returns the historical session list.

## Risks and trade-offs

- **CrashLoop on first deploy if S3 path malformed**: removing `|| true`
  trades silent data loss for loud failure. Considered correct: the chart
  user explicitly opted into backup; misconfigured backup should not
  silently degrade to no-backup.
- **Two containers share image pull**: `rclone/rclone:1.66` is pulled
  once per node; both containers share the layer. Negligible cost.
- **Sidecar idle resource usage doubled**: trivially small (`sleep
  infinity` consumes ~1MB RSS). Existing `resources` block still applies
  to both, so the requests/limits double in `kubectl describe pod`
  arithmetic; for the default values (10m / 32Mi requests) that's still
  immaterial relative to the quill container.
- **Helper template indentation foot-gun**: callers must pick the right
  `nindent` depth. Mitigated by tests #6 and #8 above (rendered output
  must contain specific env-var keys at the right path).

## Migration

Existing chart users running 2026-04-27's single-sidecar layout: a
`helm upgrade` replaces the Deployment template; rolling update creates
new pods with the two-container init layout. No state migration needed
because the S3 prefix and emptyDir contracts are unchanged. The new
`s3-restore` simply succeeds where the old single-sidecar's race-prone
copy was failing.
