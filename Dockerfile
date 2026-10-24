# syntax=docker/dockerfile:1
#
# pos-ticker -- static Go binary in a FROM scratch image.
#
# The build stage always runs on the *build* host's native architecture and
# cross-compiles with GOOS/GOARCH/GOARM, so multi-arch builds
# (linux/amd64, linux/arm64, linux/arm/v7) need no QEMU emulation.

# ── Stage 1: build ────────────────────────────────────────────────────────────
# Keep the Go minor version in step with the `go` directive in go.mod.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

# Populated automatically by buildx for the *target* platform.
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
# Stamped into the binary as main.version (set by CI: git tag or sha).
ARG VERSION=dev

# CA bundle (SMTP/TLS to Gmail) is copied into the final image from here.
RUN apk add --no-cache ca-certificates

WORKDIR /src

# Dependency layer: only re-run when go.mod / go.sum change.
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# linux/arm/v7 -> GOARM=7 (TARGETVARIANT is "v7"); no-op on other arches.
# tzdata is embedded in the binary (time/tzdata), so it is not copied here.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    if [ "${TARGETARCH}" = "arm" ]; then \
        GOARM="${TARGETVARIANT#v}"; export GOARM="${GOARM:-7}"; \
    fi; \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -buildvcs=false \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/pos-ticker ./cmd/pos-ticker

# scratch has no mkdir: prepare /data here and COPY it with the right owner.
RUN mkdir -p /out/data && chmod 0700 /out/data

# ── Stage 2: runtime ──────────────────────────────────────────────────────────
FROM scratch

ARG VERSION=dev
LABEL org.opencontainers.image.title="pos-ticker" \
      org.opencontainers.image.description="Contact-form to ESC/POS thermal printer bridge (static Go binary)" \
      org.opencontainers.image.source="https://github.com/badhrinadhbade/pos-ticker" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/pos-ticker /pos-ticker
# Owned by the runtime user so a fresh named volume inherits uid/gid 65532.
COPY --from=build --chown=65532:65532 /out/data /data

ENV DB_PATH=/data/messages.db \
    PUBLIC_ADDR=0.0.0.0:8080 \
    ADMIN_ADDR=0.0.0.0:8000

USER 65532:65532
VOLUME ["/data"]

# 8000 = admin UI/API/metrics (never route through a tunnel)
# 8080 = public contact form listener (the only thing cloudflared should reach)
EXPOSE 8000 8080

# No curl, no shell: the binary checks itself.
# Contract for the `healthcheck` subcommand: GET http://127.0.0.1:8000/health
# (port taken from ADMIN_ADDR if set), exit 0 on HTTP 200, exit 1 otherwise.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/pos-ticker", "healthcheck"]

ENTRYPOINT ["/pos-ticker"]
