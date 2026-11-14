FROM node:22.23.3-alpine AS web
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN npx --yes pnpm@10.34.7 install --frozen-lockfile
COPY docs/design/tokens.json /src/docs/design/tokens.json
COPY web/ ./
RUN npx --yes pnpm@10.34.7 build

FROM golang:1.26.9-alpine AS go
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
COPY gen gen
COPY fixtures fixtures
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/stackwright/stackwright/internal/version.Version=${VERSION}" -o /out/ ./cmd/stackwright ./cmd/stackwright-executor ./cmd/stackwright-builder
# Remote build servers download these from the builder port (adr/0021).
RUN for arch in amd64 arm64; do \
      CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags="-s -w -X github.com/stackwright/stackwright/internal/version.Version=${VERSION}" \
        -o /out/downloads/stackwright-builder-linux_$arch ./cmd/stackwright-builder; \
    done
# The CLI for engineers' machines, gzipped, with the SHA-256 of each
# binary (adr/0044).
RUN mkdir -p /out/cli && cd /out/cli && for t in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do \
      name=stackwright-${t%/*}-${t#*/}; \
      CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build -C /src -trimpath -ldflags="-s -w -X github.com/stackwright/stackwright/internal/version.Version=${VERSION}" \
        -o /out/cli/$name ./cmd/stackwright && \
      sha256sum $name >> SHA256SUMS && gzip -9 $name; \
    done
RUN mkdir -p /out/files

# The editor's language server (adr/0034), checked against the release's
# checksums.
FROM alpine:3.22.2 AS tofu-ls
ARG TARGETARCH
ARG TOFU_LS_VERSION=0.5.3
RUN case "$TARGETARCH" in \
      arm64) arch=arm64; sum=d30430f86720100e6fc6d16881fcf16bc9e4cb3c4d221fafc1aa992f01387db0 ;; \
      amd64) arch=x86_64; sum=ccf8d85881f493483c9c20ac010d320bf455fdc9dcc39ba0af515206cb488460 ;; \
      *) echo "no tofu-ls for $TARGETARCH"; exit 1 ;; \
    esac && \
    wget -q -O /tmp/tofu-ls.tar.gz "https://github.com/opentofu/tofu-ls/releases/download/v${TOFU_LS_VERSION}/tofu-ls_Linux_${arch}.tar.gz" && \
    echo "$sum  /tmp/tofu-ls.tar.gz" | sha256sum -c - && \
    tar -xzf /tmp/tofu-ls.tar.gz -C /usr/local/bin tofu-ls

# The security scanner (adr/0037), checked against the release's checksums.
FROM alpine:3.22.2 AS trivy
ARG TARGETARCH
ARG TRIVY_VERSION=0.76.1
RUN case "$TARGETARCH" in \
      arm64) arch=ARM64; sum=e174dff2a6dba5e3ae41f8d0537a6324405bd08132497340304c6b20e0f16a1f ;; \
      amd64) arch=64bit; sum=c7cbe3137fb8a48a17574bede3dd6beb16742d1e9772929940fc90082c39d9c0 ;; \
      *) echo "no trivy for $TARGETARCH"; exit 1 ;; \
    esac && \
    wget -q -O /tmp/trivy.tar.gz "https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}/trivy_${TRIVY_VERSION}_Linux-${arch}.tar.gz" && \
    echo "$sum  /tmp/trivy.tar.gz" | sha256sum -c - && \
    tar -xzf /tmp/trivy.tar.gz -C /usr/local/bin trivy

FROM alpine:3.22.2 AS terragrunt
ARG TARGETARCH
ARG TERRAGRUNT_VERSION=1.2.1
RUN case "$TARGETARCH" in \
      arm64) sum=01c0768aab01e0c183209ba7edce9d2ebc8f8a7690763bb52f1b8900aec224bc ;; \
      amd64) sum=f2f6238ad2348855a4e876b90fd31f3f5d106bb234794202a00b2bcc139fa753 ;; \
      *) echo "no terragrunt for $TARGETARCH"; exit 1 ;; \
    esac && \
    wget -q -O /usr/local/bin/terragrunt "https://github.com/gruntwork-io/terragrunt/releases/download/v${TERRAGRUNT_VERSION}/terragrunt_linux_${TARGETARCH}" && \
    echo "$sum  /usr/local/bin/terragrunt" | sha256sum -c - && \
    chmod 0755 /usr/local/bin/terragrunt

# Alpine for git, which the engines use to download most modules
# (adr/0024). The sandbox still runs as 65532 with a read-only root.
FROM alpine:3.22.2 AS executor
RUN apk add --no-cache git ca-certificates
COPY --from=tofu-ls /usr/local/bin/tofu-ls /usr/local/bin/tofu-ls
COPY --from=trivy /usr/local/bin/trivy /usr/local/bin/trivy
COPY --from=terragrunt /usr/local/bin/terragrunt /usr/local/bin/terragrunt
USER 65532:65532
COPY --from=go /out/stackwright-executor /stackwright-executor
COPY --from=go --chown=65532:65532 /out/files /run/stackwright
ENTRYPOINT ["/stackwright-executor"]

# Root, to use the Docker socket, which is root on its host anyway (adr/0007).
FROM gcr.io/distroless/static-debian12 AS builder
COPY --from=go /out/stackwright-builder /stackwright-builder
ENTRYPOINT ["/stackwright-builder"]

FROM gcr.io/distroless/static-debian12:nonroot AS binary
COPY --from=go /out/stackwright /stackwright
ENTRYPOINT ["/stackwright"]

FROM binary
COPY --from=go /out/downloads /downloads
COPY --from=go /out/cli /cli
COPY --from=web /src/web/dist /srv/web
CMD ["server", "--web", "/srv/web"]
