# Helm Chart — S3 Backup for Agent Working Directory

**Date:** 2026-04-27
**Branch:** `feat/helm-s3-backup`
**Status:** Spec — pending implementation plan

## Background

The Helm chart at `deploy/helm/quill/` mounts each instance's
`agent.workingDir` (`/home/agent` for kiro, `/home/node` for the Node-based
agents) as `emptyDir: {}` on the Deployment template. Every pod restart —
image upgrade, OOMKill, node drain — wipes the directory. That directory
holds the agent's session state on disk:

- Kiro:   `~/.kiro/sessions/cli/<uuid>.json` + `<uuid>.jsonl`
- Claude: `~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`
- Codex:  `~/.codex/history.jsonl`
- Copilot:`~/.copilot/session-state/<uuid>/workspace.yaml` + `events.jsonl`

When the directory is wiped, `/pick` returns an empty list and users lose
the ability to resume earlier conversations. This spec adds an opt-in S3
backup that restores the working directory on pod startup and uploads it
back on graceful shutdown, so session continuity survives restarts and
upgrades.

## Goals

- Pod start → directory is hydrated from S3 (no-op when bucket prefix is empty).
- Pod terminates gracefully → directory is mirrored to S3.
- One Deployment, one S3 prefix; no multi-writer coordination needed
  (`replicas: 1` is the documented constraint).
- Three AWS auth modes selectable in `values.yaml`: IRSA, EKS Pod Identity, static credentials.
- Zero modification to the four Dockerfile variants.
- Disabled by default — existing chart users see no behaviour change.

## Non-goals

- Periodic mid-flight sync (cron sidecar). Hard crashes (OOMKill, node
  failure) accept data loss back to the last successful preStop. The
  user explicitly chose this trade-off for simplicity.
- Multi-pod coordination / leader election. `replicas: 1` is enforced
  by the chart and validated in `values.yaml` schema.
- Cross-region replication, KMS encryption beyond SSE-S3, retention
  policies — all delegated to the S3 bucket configuration.
- Restore-from-snapshot UX (rolling back to a previous backup). Only the
  latest sync state is preserved.
- Backups for Discord / Telegram / Teams config — secrets and configmaps
  remain managed via Helm values, not S3.

## Architecture

```
Pod created
  ↓
[native sidecar `s3-sync` starts]              (k8s 1.28+ restartPolicy: Always)
  ↓
  rclone copy "s3:{bucket}/{prefix}/{instance}" /workdir
  ↓
sidecar idles (sleep loop)
  ↓
[main container `quill` starts]
  ↓
... agent runs, writes session state to /workdir ...
  ↓
Pod termination signal (RollingUpdate / scale-down / SIGTERM)
  ↓
[main container preStop + SIGTERM, exit]      (existing 30s graceful drain)
  ↓
[sidecar preStop hook fires AFTER main exits] (k8s 1.28+ ordering guarantee)
  ↓
  rclone sync /workdir "s3:{bucket}/{prefix}/{instance}"
  ↓
Pod fully terminated
```

`workdir` is the existing emptyDir volume mounted at
`agent.workingDir` for both containers. The sidecar mounts the same
volume at `/workdir`. No PVC is introduced — the sidecar's job is to
turn the emptyDir into something durable across pod lifecycles.

The sidecar runs `rclone/rclone:1.66`. Authentication uses rclone's
`env_auth=true` mode, which falls through to the AWS SDK credential
chain (IRSA tokens, Pod Identity sidecar, env vars, instance metadata).
A single sidecar definition covers all three auth modes; only the
`ServiceAccount` annotations and pod env vars differ per mode.

## Components

### `deploy/helm/quill/values.yaml`

Add a `backup` block (disabled by default). Per-instance overrides
inside `instances.<name>.backup` follow the same shape and merge over
the global default.

```yaml
backup:
  enabled: false
  s3:
    bucket: ""
    prefix: "quill"        # final path: s3://{bucket}/{prefix}/{instance-name}
    region: us-east-1
    endpoint: ""           # optional, for MinIO / non-AWS S3-compatible
  auth:
    # mode: irsa | podIdentity | secret
    mode: irsa
    irsa:
      roleArn: ""          # set when mode=irsa
    podIdentity:
      # No fields — assumes the cluster has Pod Identity Agent and the
      # association is provisioned out-of-band (Terraform / eksctl).
    secret:
      existingSecret: ""   # name of an existing k8s Secret with AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY
      # OR inline (if existingSecret empty, chart creates one)
      accessKeyID: ""
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

### `deploy/helm/quill/Chart.yaml`

Bump `kubeVersion: ">=1.28.0-0"` to advertise the native sidecar
requirement. Bump chart `version` (semver minor — new opt-in feature).
`appVersion` is unchanged (the Go binary did not move).

### New: `deploy/helm/quill/templates/serviceaccount.yaml`

Render one ServiceAccount per instance when backup is enabled. The
ServiceAccount carries the IRSA annotation when `auth.mode=irsa`. For
`mode=podIdentity` and `mode=secret`, the SA is plain — the
PodIdentityAssociation is created out-of-band (Terraform / eksctl), and
secret-mode pods inject env vars instead of relying on a pod-level
identity.

```yaml
{{- range $name, $inst := .Values.instances }}
{{- if and $inst.enabled (or $inst.backup $.Values.backup) }}
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

### Modified: `deploy/helm/quill/templates/deployment.yaml`

Add the native sidecar inside `initContainers` with
`restartPolicy: Always`. Wire `serviceAccountName` to the new SA.
Bump `terminationGracePeriodSeconds` to 60. Inject auth env vars and
rclone S3 config via env block.

```yaml
spec:
  terminationGracePeriodSeconds: 60
  {{- if and $b.enabled (or (eq $b.auth.mode "irsa") (eq $b.auth.mode "podIdentity")) }}
  serviceAccountName: {{ include "quill.fullname" $ }}-{{ $name }}
  {{- end }}
  initContainers:
    {{- if $b.enabled }}
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
          # Idle loop — wait for SIGTERM
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
        {{- toYaml $b.resources | nindent 8 }}
    {{- end }}
```

### New: `deploy/helm/quill/templates/secret-s3.yaml`

Render only when `auth.mode=secret` and `existingSecret` is empty.
Holds inline credentials. Marked with the `helm.sh/resource-policy:
keep` annotation so a `helm uninstall` does not orphan in-flight
backups still in progress.

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

### Modified: `deploy/helm/quill/README.md`

Add a "Session backup" section documenting:

- The K8s 1.28+ requirement and the rationale (native sidecar).
- The three auth modes with example values blocks.
- The `replicas: 1` constraint and why (no multi-writer coordination).
- A note that hard crashes (OOMKill, node failure) lose state since the
  last successful preStop — to mitigate, ensure pod resource requests
  prevent OOM and run on stable nodes.
- A pointer to AWS S3 lifecycle policies for retention.

## Error handling

| Scenario | Behaviour | UX impact |
|---|---|---|
| First deploy, bucket prefix empty | `rclone copy` exits 0, sidecar enters idle loop. | Same as today — empty `/pick`. |
| `rclone copy` fails (network, IAM) on startup | `\|\| true` in the command swallows the error so the sidecar still enters idle loop and the main container can start. The pod logs the rclone failure. | Pod still starts; user sees empty session list. Next preStop attempts to sync fresh state up. |
| `rclone sync` fails on shutdown | preStop returns non-zero; k8s logs the failure; pod still terminates. | Session state lost — same as the no-backup baseline. Surfaced in pod events for ops. |
| `terminationGracePeriodSeconds` exceeded | k8s SIGKILLs sidecar. preStop is interrupted. | Partial upload; rclone is idempotent so the next pod's restore picks up whatever made it. |
| Hard pod kill (OOMKill, node lost) | No preStop fires. State lost since previous successful preStop. | Documented limitation. |
| `auth.mode=irsa` with bad `roleArn` | rclone fails AWS API; falls into the `\|\| true` startup branch. | Pod still starts, both restore and backup fail. Surface in logs. |
| `replicas: 2` configured by user | Two pods race on `rclone sync`. Last writer wins. State corruption possible. | Out of scope — chart README documents the constraint loudly. The current `deployment.yaml` hard-codes `replicas: 1` and the chart does not expose a `replicas` value, so this misconfig is only reachable by editing the template directly. |
| Bucket region mismatch | rclone surfaces `BadRequest` / `301 Moved Permanently`. | Operator misconfig; surfaced in pod events. |

## Testing

### Hermetic

Helm chart only — Go test suite is untouched. Verification is via
`helm template` and `helm lint`:

| Test | Method |
|---|---|
| `backup.enabled: false` produces no sidecar | `helm template` and grep absence of `s3-sync` |
| `backup.enabled: true, auth.mode: irsa` produces SA with the right annotation, pod with the sidecar, and no Secret | `helm template` and grep |
| `backup.enabled: true, auth.mode: podIdentity` produces SA without IRSA annotation, no Secret | `helm template` and grep |
| `backup.enabled: true, auth.mode: secret` with inline keys produces a Secret + sidecar with valueFrom env | `helm template` and grep |
| `backup.enabled: true, auth.mode: secret` with `existingSecret: foo` produces no chart-managed Secret, sidecar references `foo` | `helm template` and grep |
| Chart syntactically valid | `helm lint deploy/helm/quill` exits 0 across all four scenarios |

A small bash test script under `deploy/helm/quill/tests/` runs all four
scenarios on each PR (CI invocation appended to `.github/workflows/`).

### Manual / smoke

After merge, on a real EKS cluster with `replicas: 1`:

1. Configure IRSA role with `s3:GetObject*`, `s3:PutObject*`,
   `s3:ListBucket`, `s3:DeleteObject*` on `s3://{bucket}/{prefix}/*`.
2. `helm upgrade --install quill ... --set backup.enabled=true ...`.
3. Send a few messages via Telegram / Discord / Teams; observe sessions
   on disk via `kubectl exec ... ls /home/agent/.kiro/sessions/cli`.
4. `kubectl rollout restart deployment/quill-kiro` and watch sidecar logs:
   - On termination: `rclone sync` line.
   - On new pod: `rclone copy` line.
5. `/pick` in the chat — historical sessions still listed.

## Definition of Done

- [ ] `helm template` produces correct output for all four auth-mode scenarios.
- [ ] `helm lint deploy/helm/quill` clean.
- [ ] Chart `version` bumped (semver minor); `kubeVersion` reflects 1.28+.
- [ ] `deploy/helm/quill/README.md` documents the feature, K8s constraint, and the three auth modes.
- [ ] Manual EKS verification: full restart cycle preserves `/pick` history.
- [ ] Backup disabled by default; existing deployments unaffected after `helm upgrade`.

## Open questions

- None at the time of writing. Decisions deferred during brainstorming
  (full directory backup, no KMS in v1, single-prefix layout) are
  documented above and may be revisited after first production use.

## References

- Kubernetes native sidecars (1.28+): <https://kubernetes.io/blog/2023/08/25/native-sidecar-containers/>
- IRSA: <https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts.html>
- EKS Pod Identity: <https://docs.aws.amazon.com/eks/latest/userguide/pod-identities.html>
- rclone S3 backend: <https://rclone.org/s3/>
- Existing Helm chart: `deploy/helm/quill/`
