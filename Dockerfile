# --- Build stage ---
FROM golang:1.26-bookworm AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.commit=${COMMIT}" -o openab-go .

# --- Runtime stage ---
FROM debian:bookworm-slim

ARG GH_CLI_VERSION=2.74.1

# Layer 1: stable system packages (rarely changes)
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl unzip procps \
    && rm -rf /var/lib/apt/lists/*

# Layer 2: kiro-cli + gh CLI (pinned versions, cacheable)
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

RUN useradd -m -s /bin/bash -u 1000 agent \
    && mkdir -p /home/agent/.local/share/kiro-cli /home/agent/.kiro \
    && chown -R agent:agent /home/agent
ENV HOME=/home/agent
WORKDIR /home/agent

COPY --from=builder --chown=agent:agent /build/openab-go /usr/local/bin/openab-go

USER agent
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD pgrep -x openab-go || exit 1
ENTRYPOINT ["openab-go"]
CMD ["/etc/openab-go/config.toml"]
