# syntax=docker/dockerfile:1

# --- build stage: current Go toolchain, static binary, no cgo ---
# $BUILDPLATFORM keeps the compile fast; $TARGETARCH cross-compiles
# the binary for the image platform (amd64/arm64).
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /discord-backup . \
 && mkdir -p /data && chown 65532:65532 /data

# --- run stage: distroless (Debian 13), non-root, ~12 MB total ---
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build --chown=65532:65532 /discord-backup /usr/local/bin/discord-backup
COPY --from=build --chown=65532:65532 /data /data
WORKDIR /data
ENTRYPOINT ["/usr/local/bin/discord-backup"]
CMD ["--help"]
