# syntax=docker/dockerfile:1.7
#
# Build:   docker build -t transcoder .
# Run:     see docker-compose.yml
#
# One image serves every encoder backend. Hardware access comes from the
# host at run time (NVIDIA container runtime for NVENC, /dev/dri for
# Intel/AMD); the image itself only needs ffmpeg with those encoders
# compiled in, which Ubuntu's ffmpeg package provides.

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS builder
ARG TARGETOS TARGETARCH VERSION=dev COMMIT= DATE=
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags="-s -w -X github.com/bughatti/transcoder/internal/version.Version=$VERSION -X github.com/bughatti/transcoder/internal/version.Commit=$COMMIT -X github.com/bughatti/transcoder/internal/version.Date=$DATE" \
      -o /out/transcoder ./cmd/transcoder

FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg \
        intel-media-va-driver-non-free \
        libva-drm2 \
        mesa-va-drivers \
        ca-certificates \
        tini \
    && rm -rf /var/lib/apt/lists/*

# NVIDIA runtime capability hints (ignored on hosts without the runtime).
ENV NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=compute,video,utility \
    TRANSCODER_CONFIG=/config/config.yaml \
    TRANSCODER_DATA_DIR=/config/data \
    TRANSCODER_LOG_DIR=/config/logs \
    TRANSCODER_TEMP_DIR=/tmp/transcoder

COPY --from=builder /out/transcoder /usr/local/bin/transcoder
COPY config.example.yaml /config.example.yaml

VOLUME ["/config"]
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=8s --start-period=40s \
    CMD ["/usr/local/bin/transcoder", "--healthcheck"]

# Starts as root, drops to PUID/PGID in-process (see internal/priv).
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/transcoder"]
CMD ["--mode=all"]
