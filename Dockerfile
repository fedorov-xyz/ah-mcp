# syntax=docker/dockerfile:1

# ---- build -----------------------------------------------------------------
# Runs on the build host's native platform and cross-compiles for the target
# (no QEMU needed for Go).
FROM --platform=$BUILDPLATFORM golang:1.27.1 AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/ah-mcp ./cmd/ah-mcp

# ---- runtime ---------------------------------------------------------------
FROM gcr.io/distroless/static:nonroot

ARG VERSION=dev
ARG COMMIT=unknown

LABEL org.opencontainers.image.title="ah-mcp" \
      org.opencontainers.image.description="Self-hostable Albert Heijn MCP server (Streamable HTTP, OAuth 2.1)" \
      org.opencontainers.image.source="https://github.com/fedorov-xyz/ah-mcp" \
      org.opencontainers.image.url="https://github.com/fedorov-xyz/ah-mcp" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"

COPY --from=build /out/ah-mcp /ah-mcp

# Inside a container the server must listen on all interfaces; publish the
# port on 127.0.0.1 (compose) or behind an ingress instead.
ENV AH_MCP_BIND=0.0.0.0 \
    AH_MCP_PORT=3000

USER nonroot:nonroot
EXPOSE 3000

# distroless has no curl: the binary checks its own /healthz.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/ah-mcp", "healthcheck"]

ENTRYPOINT ["/ah-mcp"]
