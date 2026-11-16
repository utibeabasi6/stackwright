FROM node:22.23.3-alpine@sha256:0a7108bf6c7bf5de370ffb1a3ed6be93d405b43ff159f681a8d18c0e2bc2e402 AS web
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN npx --yes pnpm@10.34.7 install --frozen-lockfile
COPY docs/design/tokens.json /src/docs/design/tokens.json
COPY web/ ./
RUN npx --yes pnpm@10.34.7 build
# The bundle ships its packages' code, so it ships their licences too.
RUN npx --yes pnpm@10.34.7 licenses list --prod --json | node scripts/notices.mjs > dist/THIRD_PARTY_NOTICES.txt

FROM golang:1.26.11-alpine@sha256:ae52b5bc6ed045a9ed26ab03dc19327a802a9cc9aa8b27ed57f5c6b60579b0bb AS go
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
COPY gen gen
COPY fixtures fixtures
COPY tools tools
# The licences, notices and, for MPL-2.0, the source of the Go modules that
# the binaries hold.
RUN go tool -modfile=tools/go.mod go-licenses save ./cmd/... --save_path=/out/licenses/go --ignore github.com/utibeabasi6/stackwright
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/utibeabasi6/stackwright/internal/version.Version=${VERSION}" -o /out/ ./cmd/stackwright ./cmd/stackwright-executor ./cmd/stackwright-builder
# Remote build servers download these from the builder port (adr/0021).
RUN for arch in amd64 arm64; do \
      CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags="-s -w -X github.com/utibeabasi6/stackwright/internal/version.Version=${VERSION}" \
        -o /out/downloads/stackwright-builder-linux_$arch ./cmd/stackwright-builder; \
    done
# The CLI for engineers' machines, gzipped, with the SHA-256 of each
# binary (adr/0044).
RUN mkdir -p /out/cli && cd /out/cli && for t in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do \
      name=stackwright-${t%/*}-${t#*/}; \
      CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build -C /src -trimpath -ldflags="-s -w -X github.com/utibeabasi6/stackwright/internal/version.Version=${VERSION}" \
        -o /out/cli/$name ./cmd/stackwright && \
      sha256sum $name >> SHA256SUMS && gzip -9 $name; \
    done
RUN mkdir -p /out/files

# The editor's language server (adr/0034), checked against the release's
# checksums.
FROM alpine:3.22.2@sha256:4b7ce07002c69e8f3d704a9c5d6fd3053be500b7f1c69fc0d80990c2ad8dd412 AS tofu-ls
ARG TARGETARCH
ARG TOFU_LS_VERSION=0.5.3
RUN case "$TARGETARCH" in \
      arm64) arch=arm64; sum=d30430f86720100e6fc6d16881fcf16bc9e4cb3c4d221fafc1aa992f01387db0 ;; \
      amd64) arch=x86_64; sum=ccf8d85881f493483c9c20ac010d320bf455fdc9dcc39ba0af515206cb488460 ;; \
      *) echo "no tofu-ls for $TARGETARCH"; exit 1 ;; \
    esac && \
    wget -q -O /tmp/tofu-ls.tar.gz "https://github.com/opentofu/tofu-ls/releases/download/v${TOFU_LS_VERSION}/tofu-ls_Linux_${arch}.tar.gz" && \
    echo "$sum  /tmp/tofu-ls.tar.gz" | sha256sum -c - && \
    tar -xzf /tmp/tofu-ls.tar.gz -C /usr/local/bin tofu-ls && \
    mkdir -p /licenses/tofu-ls && \
    wget -q -O /licenses/tofu-ls/LICENSE "https://raw.githubusercontent.com/opentofu/tofu-ls/v${TOFU_LS_VERSION}/LICENSE"

# The security scanner (adr/0037), checked against the release's checksums.
FROM alpine:3.22.2@sha256:4b7ce07002c69e8f3d704a9c5d6fd3053be500b7f1c69fc0d80990c2ad8dd412 AS trivy
ARG TARGETARCH
ARG TRIVY_VERSION=0.76.1
RUN case "$TARGETARCH" in \
      arm64) arch=ARM64; sum=e174dff2a6dba5e3ae41f8d0537a6324405bd08132497340304c6b20e0f16a1f ;; \
      amd64) arch=64bit; sum=c7cbe3137fb8a48a17574bede3dd6beb16742d1e9772929940fc90082c39d9c0 ;; \
      *) echo "no trivy for $TARGETARCH"; exit 1 ;; \
    esac && \
    wget -q -O /tmp/trivy.tar.gz "https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}/trivy_${TRIVY_VERSION}_Linux-${arch}.tar.gz" && \
    echo "$sum  /tmp/trivy.tar.gz" | sha256sum -c - && \
    tar -xzf /tmp/trivy.tar.gz -C /usr/local/bin trivy && \
    mkdir -p /licenses/trivy && \
    wget -q -O /licenses/trivy/LICENSE "https://raw.githubusercontent.com/aquasecurity/trivy/v${TRIVY_VERSION}/LICENSE"

FROM alpine:3.22.2@sha256:4b7ce07002c69e8f3d704a9c5d6fd3053be500b7f1c69fc0d80990c2ad8dd412 AS terragrunt
ARG TARGETARCH
ARG TERRAGRUNT_VERSION=1.2.1
RUN case "$TARGETARCH" in \
      arm64) sum=01c0768aab01e0c183209ba7edce9d2ebc8f8a7690763bb52f1b8900aec224bc ;; \
      amd64) sum=f2f6238ad2348855a4e876b90fd31f3f5d106bb234794202a00b2bcc139fa753 ;; \
      *) echo "no terragrunt for $TARGETARCH"; exit 1 ;; \
    esac && \
    wget -q -O /usr/local/bin/terragrunt "https://github.com/gruntwork-io/terragrunt/releases/download/v${TERRAGRUNT_VERSION}/terragrunt_linux_${TARGETARCH}" && \
    echo "$sum  /usr/local/bin/terragrunt" | sha256sum -c - && \
    chmod 0755 /usr/local/bin/terragrunt && \
    mkdir -p /licenses/terragrunt && \
    wget -q -O /licenses/terragrunt/LICENSE.txt "https://raw.githubusercontent.com/gruntwork-io/terragrunt/v${TERRAGRUNT_VERSION}/LICENSE.txt"

# Alpine for git, which the engines use to download most modules
# (adr/0024). The sandbox still runs as 65532 with a read-only root.
FROM alpine:3.22.2@sha256:4b7ce07002c69e8f3d704a9c5d6fd3053be500b7f1c69fc0d80990c2ad8dd412 AS executor
RUN apk add --no-cache git ca-certificates
COPY --from=tofu-ls /usr/local/bin/tofu-ls /usr/local/bin/tofu-ls
COPY --from=trivy /usr/local/bin/trivy /usr/local/bin/trivy
COPY --from=terragrunt /usr/local/bin/terragrunt /usr/local/bin/terragrunt
COPY --from=tofu-ls /licenses /licenses
COPY --from=trivy /licenses /licenses
COPY --from=terragrunt /licenses /licenses
COPY --from=go /out/licenses /licenses
COPY LICENSE /licenses/stackwright/LICENSE
USER 65532:65532
COPY --from=go /out/stackwright-executor /stackwright-executor
# Root owns the files folder, so job code can read its files but cannot
# write to the folder, which is a volume with no size limit.
COPY --from=go /out/files /run/stackwright
ENTRYPOINT ["/stackwright-executor"]

# Root, to use the Docker socket, which is root on its host anyway (adr/0007).
FROM gcr.io/distroless/static-debian12:latest@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2 AS builder
COPY --from=go /out/stackwright-builder /stackwright-builder
COPY --from=go /out/licenses /licenses
COPY LICENSE /licenses/stackwright/LICENSE
ENTRYPOINT ["/stackwright-builder"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS binary
COPY --from=go /out/stackwright /stackwright
COPY --from=go /out/licenses /licenses
COPY LICENSE /licenses/stackwright/LICENSE
ENTRYPOINT ["/stackwright"]

FROM binary
COPY --from=go /out/downloads /downloads
COPY --from=go /out/cli /cli
COPY --from=web /src/web/dist /srv/web
CMD ["server", "--web", "/srv/web"]
