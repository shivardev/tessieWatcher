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

# distroless/static (not `scratch`) so HTTPS to Tesla's SSO/Owner API
# has a CA certificate bundle to validate against.
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/teslalog /usr/local/bin/teslalog
COPY config.example.toml /etc/teslalog/config.toml
VOLUME ["/var/lib/teslalog"]
ENV TESLALOG_CONFIG=/etc/teslalog/config.toml
ENTRYPOINT ["/usr/local/bin/teslalog"]
CMD ["run"]
