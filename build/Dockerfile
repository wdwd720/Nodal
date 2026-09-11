# syntax=docker/dockerfile:1
# Shared multi-stage build for every cmd/<name> binary. Built by scripts/images:
#   docker build -f build/Dockerfile --build-arg CMD=api --build-arg VERSION=<sha> --build-arg REVISION=<sha> -t controlplane/api:<sha> .
# The runtime image is distroless static, non-root (uid 65532), needs no writable
# filesystem, and exposes a single fixed entrypoint /app/controlplane.

ARG GO_IMAGE=golang:1.27-alpine
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

# ---------------------------------------------------------------------------
# builder
# ---------------------------------------------------------------------------
FROM ${GO_IMAGE} AS builder
ARG CMD
# VERSION is the string linked into config.BuildVersion and reported by
# /v1/version, which exists to prove which build is running.
#
# Render passes a service's environment variables to `docker build` as build
# args and sets RENDER_GIT_COMMIT itself, at build time and at run time. So the
# default below is the commit Render is building when Render is the builder,
# and "dev" everywhere else -- a local `docker build` with no --build-arg is
# unchanged. scripts/images still passes --build-arg VERSION=<sha> explicitly,
# which wins over the default.
#
# Before this, every image built from render.yaml reported build_version "dev",
# and because BuildVersion is part of Config.Hash the hash could not tell two
# builds of the same configuration apart either (F-142).
ARG RENDER_GIT_COMMIT=
ARG VERSION=${RENDER_GIT_COMMIT:-dev}
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly

WORKDIR /src

# Dependencies first so the module cache layer survives source changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download && go mod verify

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    test -n "${CMD}" || { echo "build-arg CMD is required (a directory under cmd/)"; exit 1; } && \
    GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH:-amd64}" \
    go build -trimpath \
      -ldflags="-s -w -X github.com/nodal/controlplane/internal/config.BuildVersion=${VERSION}" \
      -o /out/controlplane "./cmd/${CMD}"

# ---------------------------------------------------------------------------
# runtime
# ---------------------------------------------------------------------------
FROM ${RUNTIME_IMAGE}
ARG CMD
# The same two lines again: an ARG declared inside a stage is scoped to it, so
# the OCI version label would otherwise say "dev" on an image whose binary says
# the commit.
ARG RENDER_GIT_COMMIT=
ARG VERSION=${RENDER_GIT_COMMIT:-dev}
ARG REVISION=unknown
ARG CREATED=unknown

LABEL org.opencontainers.image.title="${CMD}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.created="${CREATED}" \
      org.opencontainers.image.source="https://github.com/nodal/controlplane" \
      org.opencontainers.image.base.name="gcr.io/distroless/static-debian12:nonroot" \
      org.opencontainers.image.description="Universal Financial Control Plane: ${CMD}"

WORKDIR /app
COPY --from=builder --chown=65532:65532 /out/controlplane /app/controlplane

# Numeric UID/GID so Kubernetes runAsNonRoot can verify the user without a passwd lookup.
USER 65532:65532
ENV TZ=UTC

# Read-only root filesystem friendly: nothing is written under /app; use a tmpfs
# at /tmp if a command needs scratch space.
ENTRYPOINT ["/app/controlplane"]
