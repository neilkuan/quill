# --- Build stage ---
FROM golang:1.26-bookworm AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.commit=${COMMIT}" -o quill .

# --- AWS CLI source stage (official upstream image, always latest v2) ---
FROM public.ecr.aws/aws-cli/aws-cli:latest AS aws-source

# --- Runtime stage ---
FROM debian:bookworm-slim

ARG GH_CLI_VERSION=2.91.0
# kiro-cli is pinned by version + SHA256. AWS publishes versioned URLs and
# per-zip .sha256 files at https://desktop-release.q.us-east-1.amazonaws.com/<ver>/
# and a manifest at /latest/manifest.json. To upgrade: run scripts/update-kiro-cli.sh
# which rewrites these three ARGs. Pinning keeps this layer cacheable across
# builds — it only invalidates when the pin is intentionally bumped.
ARG KIRO_CLI_VERSION=2.1.0
ARG KIRO_CLI_SHA256_AMD64=97b5bc8b79b43a6f9f43dff7cfca87d4db46028fd15c273fb75d4f91e238bae5
ARG KIRO_CLI_SHA256_ARM64=2f93836a6c1de55cb9f560e40ca44fcd5c3384a6ab64bb10f308b10ee793840e

# Layer 1: stable system packages (rarely changes)
# tini is needed as PID 1 so zombie children spawned by the agent (e.g.
# `bash -c ...` tool calls) get reaped — without it Go's quill process ends up
# as PID 1 but does not handle SIGCHLD, so zombies accumulate.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl unzip procps git tini \
    && rm -rf /var/lib/apt/lists/*

# Layer 2: kiro-cli + gh CLI (invalidated only when pinned versions/SHAs change)
RUN ARCH=$(dpkg --print-architecture) \
    && if [ "$ARCH" = "arm64" ]; then \
         KIRO_ARCH="aarch64"; EXPECTED_SHA="${KIRO_CLI_SHA256_ARM64}"; \
       else \
         KIRO_ARCH="x86_64"; EXPECTED_SHA="${KIRO_CLI_SHA256_AMD64}"; \
       fi \
    && KIRO_URL="https://desktop-release.q.us-east-1.amazonaws.com/${KIRO_CLI_VERSION}/kirocli-${KIRO_ARCH}-linux.zip" \
    && curl --proto '=https' --tlsv1.2 -sSf --retry 3 --retry-delay 5 "$KIRO_URL" -o /tmp/kirocli.zip \
    && echo "${EXPECTED_SHA}  /tmp/kirocli.zip" | sha256sum -c - \
    && unzip -q /tmp/kirocli.zip -d /tmp \
    && cp /tmp/kirocli/bin/* /usr/local/bin/ \
    && chmod +x /usr/local/bin/kiro-cli* \
    && curl -sSL "https://github.com/cli/cli/releases/download/v${GH_CLI_VERSION}/gh_${GH_CLI_VERSION}_linux_${ARCH}.tar.gz" \
       | tar xz -C /tmp \
    && mv /tmp/gh_${GH_CLI_VERSION}_linux_${ARCH}/bin/gh /usr/local/bin/gh \
    && rm -rf /tmp/*

# Layer 3: aws CLI v2 — copied from the official upstream image.
# NOTE: only copy /usr/local/aws-cli/ (its internal symlinks are relative and
# survive COPY). The PATH-level symlink must be recreated via `ln -s` below —
# copying /usr/local/bin/aws with COPY turns the symlink into a real file,
# after which PyInstaller resolves /proc/self/exe to /usr/local/bin where the
# bundled libpython*.so isn't found, breaking the CLI at runtime.
COPY --from=aws-source /usr/local/aws-cli/ /usr/local/aws-cli/
RUN ln -s /usr/local/aws-cli/v2/current/bin/aws /usr/local/bin/aws \
    && ln -s /usr/local/aws-cli/v2/current/bin/aws_completer /usr/local/bin/aws_completer

RUN useradd -m -s /bin/bash -u 1000 agent \
    && mkdir -p /home/agent/.local/share/kiro-cli /home/agent/.kiro \
    && chown -R agent:agent /home/agent
ENV HOME=/home/agent
WORKDIR /home/agent

COPY --from=builder --chown=agent:agent /build/quill /usr/local/bin/quill

USER agent
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD curl -sf http://localhost:8080/api/health || exit 1
ENTRYPOINT ["/usr/bin/tini", "--", "quill"]
CMD ["/etc/quill/config.toml"]
