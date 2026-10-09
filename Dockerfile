# syntax=docker/dockerfile:1
#
# Two stages: a big "build" image with the Go toolchain compiles the binary,
# then a small "runtime" image gets only the binary. The final image stays tiny
# (a few tens of MB) because the compiler never ships. See docs/docker.md.

# ---- Stage 1: build ---------------------------------------------------------
# $BUILDPLATFORM is the machine doing the build (e.g. your amd64 laptop).
# Building natively and cross-compiling with GOARCH is much faster than
# emulating an arm64 CPU. Keep the Go version in step with go.mod.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

# Filled in by Docker: the platform the image is being built FOR
# (linux/amd64 for a PC, linux/arm64 for a Raspberry Pi 4/5).
ARG TARGETOS TARGETARCH

WORKDIR /src

# Download dependencies first, in their own layer. Docker caches each step, so
# as long as go.mod/go.sum don't change, rebuilds skip this.
COPY go.mod go.sum ./
RUN go mod download

# Now the source. CGO_ENABLED=0 gives a static binary with no C library
# needed at runtime (our SQLite driver is pure Go, so this works).
# -tags timetzdata embeds the time zone database, so TZ=Asia/Taipei works
# without installing anything in the runtime image.
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -tags timetzdata -ldflags="-s -w" -o /out/radar ./cmd/radar

# ---- Stage 2: runtime -------------------------------------------------------
FROM alpine:3.22

# Alpine gives us a shell (for the loop in compose.yaml) in ~8 MB.
# HTTPS needs the list of trusted certificate authorities; borrow the one
# from the build image instead of downloading it again.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
# So "today" means a Taiwan trading day, and log times read as local time.
ENV TZ=Asia/Taipei

# The app runs from /app, so the relative paths in radar.toml
# (data/radar.db, public/) land in /app/data and /app/public, which
# compose.yaml mounts from the host.
WORKDIR /app
COPY --from=build /out/radar /usr/local/bin/radar
# A default config baked in; compose.yaml mounts your own copy over it.
COPY radar.toml ./

# `docker run IMAGE` with no arguments prints usage; compose sets the command.
ENTRYPOINT ["radar"]
