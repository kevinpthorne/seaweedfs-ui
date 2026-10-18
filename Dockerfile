# syntax=docker/dockerfile:1.7

# Stage 1: Official SeaweedFS image for the `weed` CLI binary
ARG SEAWEEDFS_VERSION=3.80
FROM chrislusf/seaweedfs:${SEAWEEDFS_VERSION} AS weed-cli

# Stage 2: Build the Go server with embedded web assets
FROM golang:1.23-alpine AS builder
ARG VERSION=0.1.0
ARG GIT_COMMIT=dev
ARG BUILD_DATE=unknown

WORKDIR /workspace
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION} -X main.GitCommit=${GIT_COMMIT} -X main.BuildDate=${BUILD_DATE}" \
    -o /out/seaweedfs-ui ./cmd/seaweedfs-ui

# Stage 3: Minimal hardened runtime
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -g 65532 -S nonroot && \
    adduser -u 65532 -S nonroot -G nonroot && \
    mkdir -p /tmp/seaweedfs/.seaweedfs /etc/seaweedfs-ui && \
    chown -R 65532:65532 /tmp/seaweedfs /etc/seaweedfs-ui && \
    chmod 0700 /tmp/seaweedfs

COPY --from=weed-cli /usr/bin/weed /usr/local/bin/weed
COPY --from=builder /out/seaweedfs-ui /usr/local/bin/seaweedfs-ui
RUN chmod 0755 /usr/local/bin/weed /usr/local/bin/seaweedfs-ui && \
    ln -sf /usr/local/bin/weed /usr/bin/weed

ENV HOME=/tmp/seaweedfs \
    WEED_BINARY_PATH=/usr/local/bin/weed
USER 65532:65532
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/seaweedfs-ui"]
CMD ["-config", "/etc/seaweedfs-ui/config.yaml"]
