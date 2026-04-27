# Quill Helm Chart

Deploy Quill to EKS with ALB Ingress for Microsoft Teams webhook.

## Quick Start

```bash
# 1. Install with your secrets
helm install quill deploy/helm/quill \
  -n quill --create-namespace \
  --set secrets.TEAMS_APP_ID="<app-id>" \
  --set secrets.TEAMS_APP_SECRET="<secret>" \
  --set secrets.TEAMS_TENANT_ID="<tenant>" \
  --set ingress.host="quill.example.com" \
  --set ingress.annotations."alb\.ingress\.kubernetes\.io/certificate-arn"="arn:aws:acm:..."

# 2. Auth kiro-cli
kubectl -n quill exec -it deploy/quill-quill -- kiro-cli login --use-device-flow

# 3. Verify
curl https://quill.example.com/api/health
```

## Prerequisites

- EKS cluster with [AWS Load Balancer Controller](https://kubernetes-sigs.github.io/aws-load-balancer-controller/)
- ACM certificate for your domain
- DNS record (CNAME/Alias) pointing to the ALB

## Architecture

```
Internet → ALB (HTTPS:443)
  ├─ /api/messages → Pod:3978  (Teams Bot Framework webhook)
  └─ /api/*        → Pod:8080  (Health + session monitoring)
```

## Key Values

| Value | Default | Description |
|-------|---------|-------------|
| `image.tag` | `appVersion` | Image tag (e.g. `v0.17.0`) |
| `teams.enabled` | `true` | Enable Teams adapter |
| `discord.enabled` | `false` | Enable Discord adapter |
| `telegram.enabled` | `false` | Enable Telegram adapter |
| `ingress.enabled` | `true` | Create ALB Ingress |
| `ingress.host` | `""` | Your domain |
| `secrets.*` | `""` | Platform tokens |

## Upgrade

```bash
helm upgrade quill deploy/helm/quill -n quill --reuse-values \
  --set image.tag="v0.18.0"
```

Config changes auto-trigger pod rollout via checksum annotation.

## Multi-platform

Enable Discord/Telegram alongside Teams:

```bash
helm upgrade quill deploy/helm/quill -n quill --reuse-values \
  --set discord.enabled=true \
  --set secrets.DISCORD_BOT_TOKEN="<token>" \
  --set discord.allowedChannels="{1234567890}"
```

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
