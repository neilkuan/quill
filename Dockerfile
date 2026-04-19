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

ARG GH_CLI_VERSION=2.90.0
# Change KIRO_CLI_CACHE_BUST to force re-download of kiro-cli (no versioned URL available)
ARG KIRO_CLI_CACHE_BUST=2026-04-14

# Layer 1: stable system packages (rarely changes)
# tini is needed as PID 1 so zombie children spawned by the agent (e.g.
# `bash -c ...` tool calls) get reaped — without it Go's quill process ends up
# as PID 1 but does not handle SIGCHLD, so zombies accumulate.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl unzip procps git tini \
    && rm -rf /var/lib/apt/lists/*

# Layer 2: kiro-cli + gh CLI (cache invalidated by KIRO_CLI_CACHE_BUST)
RUN ARCH=$(dpkg --print-architecture) \
    && if [ "$ARCH" = "arm64" ]; then \
         KIRO_URL="https://desktop-release.q.us-east-1.amazonaws.com/latest/kirocli-aarch64-linux.zip"; \
       else \
         KIRO_URL="https://desktop-release.q.us-east-1.amazonaws.com/latest/kirocli-x86_64-linux.zip"; \
       fi \
    && curl --proto '=https' --tlsv1.2 -sSf --retry 3 --retry-delay 5 "$KIRO_URL" -o /tmp/kirocli.zip \
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
