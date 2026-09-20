# dop-core — ONE image, FOUR modes (ADR-0012).
# The mode is the first argument: serve | worker | sched | launcher.
# One artifact, one pipeline: what runs in production is the same binary as locally.

# ── build ────────────────────────────────────────────────────────────────────
FROM golang:1.27-alpine AS build

WORKDIR /src

# The dependencies change far less than the code: downloading before copying the
# source preserves the module layer between builds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# The version is stamped into the binary (`dop-core version`, the trace
# resource); the build-arg is what dop-infra's image-core passes.
ARG VERSION=dev

# CGO off: a static binary, it runs in an image with no system libc and does not
# depend on the C resolver (the pure-Go resolver works with the cluster's DNS).
# -trimpath takes the build machine's path out of the binary — a reproducible build.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/dop-core ./cmd/dop-core

# ── runtime ──────────────────────────────────────────────────────────────────
FROM alpine:3.22

ARG VERSION=dev
LABEL org.opencontainers.image.title="dop-core" \
      org.opencontainers.image.version="${VERSION}"

# ca-certificates: TLS with the Kubernetes API and with Firebase/GCS.
# tzdata: the scheduler compares times; with no timezone database everything
# silently becomes UTC.
RUN apk add --no-cache ca-certificates tzdata git

COPY --from=build /out/dop-core /app/dop-core

# An arbitrary non-root user (an OKD/OpenShift requirement — the substrate spec §2).
# OKD assigns a UID that does NOT exist in /etc/passwd and puts the process in
# group 0; that is why the permission lives on the root group, never on a named user.
ENV HOME=/app
RUN chgrp -R 0 /app && chmod -R g=u /app

WORKDIR /app
USER 1001
EXPOSE 9090 9091

ENTRYPOINT ["/app/dop-core"]
# With no declared mode the container serves gRPC; the Deployment overrides it with args.
CMD ["serve"]
