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
