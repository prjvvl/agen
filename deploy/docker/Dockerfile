# One image with the agen CLI/platform binary, agen-host and the MCP test
# server (used by the cluster e2e). Build from the repo root:
#   docker build -f deploy/docker/Dockerfile -t agen:dev .
FROM rust:1-bookworm AS rust
WORKDIR /src
COPY Cargo.toml Cargo.lock rustfmt.toml ./
COPY engine engine
COPY spec spec
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/src/target \
    cargo build --release -p agen-host -p agen-testkit --bins \
 && mkdir -p /out && cp target/release/agen-host target/release/mcp-test-server /out/

FROM node:24-bookworm AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web ./
RUN npx tsc --noEmit && npx vite build --outDir /out/dist --emptyOutDir

FROM golang:1.26-bookworm AS go
WORKDIR /src
COPY platform platform
COPY --from=web /out/dist platform/internal/ui/dist
RUN --mount=type=cache,target=/root/go/pkg/mod \
    cd platform && GOWORK=off CGO_ENABLED=0 go build -trimpath -o /out/agen ./cmd/agen

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
COPY --from=rust /out/agen-host /out/mcp-test-server /usr/local/bin/
COPY --from=go /out/agen /usr/local/bin/agen
COPY examples/bundles /opt/agen/examples/bundles
ENV AGEN_HOST_BIN=/usr/local/bin/agen-host AGEN_HOME=/var/lib/agen
RUN useradd --system --uid 10001 --home /var/lib/agen --create-home agen && mkdir -p /shared && chown agen /shared
USER 10001
ENTRYPOINT ["agen"]
