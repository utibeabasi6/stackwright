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

FROM gcr.io/distroless/static-debian12:nonroot AS executor
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
