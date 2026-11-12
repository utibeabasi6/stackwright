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

# Alpine for git, which the engines use to download most modules
# (adr/0024). The sandbox still runs as 65532 with a read-only root.
FROM alpine:3.22.2 AS executor
RUN apk add --no-cache git ca-certificates
COPY --from=tofu-ls /usr/local/bin/tofu-ls /usr/local/bin/tofu-ls
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
COPY --from=web /src/web/dist /srv/web
CMD ["server", "--web", "/srv/web"]
