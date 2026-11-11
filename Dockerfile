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
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/stackwright/stackwright/internal/version.Version=${VERSION}" -o /out/stackwright ./cmd/stackwright

FROM gcr.io/distroless/static-debian12:nonroot AS binary
COPY --from=go /out/stackwright /stackwright
ENTRYPOINT ["/stackwright"]

FROM binary
COPY --from=web /src/web/dist /srv/web
CMD ["server", "--web", "/srv/web"]
