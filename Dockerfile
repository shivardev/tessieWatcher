# TeslaLog Mini runs equally well with or without Docker (see README).
# This image is optional - systemd + the bare binary is the primary,
# lighter-weight deployment path for the Pi Zero 2 W. Docker is
# supported for people who'd rather manage it like any other container.
#
# Build:  docker build -t teslalog .
# Or for the Pi directly from a dev machine:
#   docker buildx build --platform linux/arm64 -t teslalog:arm64 --load .

FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=0: storage uses ncruces/go-sqlite3 (SQLite compiled to
# WebAssembly, run via the pure-Go wazero runtime), not cgo - so this
# cross-compiles for any TARGETARCH with no C toolchain at all.
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/teslalog ./cmd/teslalog
# The final stage's distroless:nonroot image has no shell, so directories
# that need to be owned by its uid/gid 65532 ("nonroot") have to be
# prepared here, in a stage that has one.
RUN mkdir -p /out/var-lib-teslalog && chown 65532:65532 /out/var-lib-teslalog

# distroless/static (not `scratch`) so HTTPS to Tesla's SSO/Owner API has a
# CA certificate bundle to validate against. The :nonroot variant runs as
# an unprivileged uid/gid (65532) rather than root, matching the hardened,
# non-root posture systemd/teslalog.service already uses on the bare-metal
# deployment path.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/teslalog /usr/local/bin/teslalog
COPY --chown=65532:65532 config.example.toml /etc/teslalog/config.toml
COPY --from=build --chown=65532:65532 /out/var-lib-teslalog /var/lib/teslalog
# Both are named volumes in docker-compose.yml so config edits (VIN,
# intervals, charging cost/efficiency, etc.) AND data survive
# `docker compose up --force-recreate` / image rebuilds, not just data.
VOLUME ["/var/lib/teslalog", "/etc/teslalog"]
ENV TESLALOG_CONFIG=/etc/teslalog/config.toml
USER nonroot
ENTRYPOINT ["/usr/local/bin/teslalog"]
CMD ["run"]
