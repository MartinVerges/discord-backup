# syntax=docker/dockerfile:1

# --- build stage: full Go toolchain, static binary, no cgo ---
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /discord-backup . \
 && mkdir -p /data && chown 65532:65532 /data

# --- run stage: distroless, non-root, ~12 MB total ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build --chown=65532:65532 /discord-backup /usr/local/bin/discord-backup
COPY --from=build --chown=65532:65532 /data /data
WORKDIR /data
ENTRYPOINT ["/usr/local/bin/discord-backup"]
CMD ["--help"]
