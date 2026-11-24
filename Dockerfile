# One image, two roles. The runner needs the claude CLI and the read-only
# cluster tools the agent uses (kubectl, flux, kustomize, git, curl, jq). The
# controller needs git, bash, yq, kustomize and flux: it runs the GitOps
# repo's own .githooks on every patch the agent proposes. Shipping both roles in one image
# keeps one release, one pin in k3s-cluster and one Renovate PR per bump.
#
# Base images are pinned by digest. Tool versions are ARGs carrying a
# `# renovate:` comment, which config:best-practices' Dockerfile manager bumps.

FROM golang:1.27@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/alert-agent ./cmd/alert-agent

# Cluster CLIs, each checked against its upstream checksum file. Verifying
# against a file fetched from the same release is weaker than a pinned hash,
# but it survives a Renovate bump without a hand edit, and it still catches a
# corrupt or truncated download.
FROM golang:1.27@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS tools

# renovate: datasource=github-releases depName=kubernetes/kubernetes
ARG KUBECTL_VERSION=v1.36.6
# renovate: datasource=github-releases depName=fluxcd/flux2
ARG FLUX_VERSION=v2.10.1
# renovate: datasource=github-releases depName=kubernetes-sigs/kustomize extractVersion=^kustomize/(?<version>v.+)$
ARG KUSTOMIZE_VERSION=v5.8.3
# renovate: datasource=github-releases depName=mikefarah/yq
ARG YQ_VERSION=v4.54.3

WORKDIR /dl
RUN set -eux; \
    curl -fsSLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"; \
    echo "$(curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl.sha256")  kubectl" | sha256sum -c -; \
    f="flux_${FLUX_VERSION#v}_linux_amd64.tar.gz"; \
    curl -fsSLO "https://github.com/fluxcd/flux2/releases/download/${FLUX_VERSION}/${f}"; \
    curl -fsSL "https://github.com/fluxcd/flux2/releases/download/${FLUX_VERSION}/flux_${FLUX_VERSION#v}_checksums.txt" | grep " ${f}\$" | sha256sum -c -; \
    tar -xzf "$f" flux; \
    f="kustomize_${KUSTOMIZE_VERSION}_linux_amd64.tar.gz"; \
    curl -fsSLO "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2F${KUSTOMIZE_VERSION}/${f}"; \
    curl -fsSL "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2F${KUSTOMIZE_VERSION}/checksums.txt" | grep " ${f}\$" | sha256sum -c -; \
    tar -xzf "$f" kustomize; \
    curl -fsSLo yq "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_amd64"; \
    echo "$(curl -fsSL "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/checksums-bsd" | sed -n 's/^SHA256 (yq_linux_amd64) = //p')  yq" | sha256sum -c -; \
    install -m 0755 kubectl flux kustomize yq /usr/local/bin/

# The claude CLI. The npm package is a thin launcher; the platform package
# carries the native binary, so only that is fetched and the runtime image
# needs no Node. `npm pack` checks the tarball against the registry's
# integrity hash.
FROM node:24-slim@sha256:873f586f3794f52281bd039bf02c048a0be08fde619338f9941bad1eb6b34530 AS claude

# renovate: datasource=npm depName=@anthropic-ai/claude-code-linux-x64
ARG CLAUDE_CODE_VERSION=2.1.331

WORKDIR /dl
RUN set -eux; \
    npm pack --silent "@anthropic-ai/claude-code-linux-x64@${CLAUDE_CODE_VERSION}"; \
    tar -xzf anthropic-ai-claude-code-linux-x64-*.tgz package/claude; \
    install -m 0755 package/claude /usr/local/bin/claude

FROM debian:trixie-slim@sha256:8aa5a7bf5a1955bddbe7d74fcdd0b91ab30e74362a8b73af672b0481b35b44a0

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl git jq; \
    rm -rf /var/lib/apt/lists/*; \
    groupadd --gid 65532 nonroot; \
    useradd --uid 65532 --gid 65532 --home-dir /home/nonroot --create-home --shell /usr/sbin/nologin nonroot

COPY --from=tools /usr/local/bin/kubectl /usr/local/bin/flux /usr/local/bin/kustomize /usr/local/bin/yq /usr/local/bin/
COPY --from=claude /usr/local/bin/claude /usr/local/bin/claude
COPY --from=build /out/alert-agent /usr/local/bin/alert-agent

# The root filesystem is read-only at runtime; the runner Job mounts an
# emptyDir at HOME for claude's config and session files. The CLI must never
# update itself in place: the version is whatever Renovate pinned above.
ENV HOME=/home/nonroot \
    DISABLE_AUTOUPDATER=1

EXPOSE 8080 8081 8082
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/alert-agent"]
CMD ["controller"]
