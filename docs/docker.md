# Docker

Prebuilt images (amd64 + arm64), published by the Docker workflow:

- `ghcr.io/martinverges/discord-backup:latest` – built from every `main` commit
- `ghcr.io/martinverges/discord-backup:v0.1.0` – built from the `v0.1.0` tag

(Binaries for Linux/macOS/Windows live on the Releases page; the Release
workflow builds those, images are published separately.)

```bash
docker pull ghcr.io/martinverges/discord-backup:latest

docker run --rm \
  --env-file .env \
  -v "$PWD/archive:/data" \
  ghcr.io/martinverges/discord-backup:latest --guild YOUR_SERVER_ID --out /data --download-attachments
```

No release yet or want to build yourself? Minimal image (~16 MB): static Go binary on
distroless (Debian 13), running as non-root (uid 65532).
The tool is a one-shot cron job, so the container runs one sync per invocation – schedule it
from the host (cron/systemd), don't keep it running.

## Build

```bash
docker build -t discord-backup .
```

## Run

```bash
mkdir -p archive
# bind mount: host directory must be writable for uid 65532 (or use --user, see below)
sudo chown 65532:65532 archive

docker run --rm \
  --env-file .env \
  -v "$PWD/archive:/data" \
  discord-backup --guild YOUR_SERVER_ID --out /data --download-attachments
```

`.env` (never commit):

```
DISCORD_TOKEN=your_bot_token_here
```

Alternative without chown – run as your own uid:

```bash
docker run --rm --user "$(id -u):$(id -g)" \
  --env-file .env \
  -v "$PWD/archive:/data" \
  discord-backup --guild YOUR_SERVER_ID --out /data --download-attachments
```

Token file instead of env:

```bash
chmod 600 token
docker run --rm \
  -v "$PWD/archive:/data" \
  -v "$PWD/token:/token:ro" \
  discord-backup --guild YOUR_SERVER_ID --out /data --token-file /token
```

## Cron (host)

```
*/15 * * * * /usr/bin/docker run --rm --env-file /etc/discord-backup/env -v /var/lib/discord-backup:/data discord-backup --guild YOUR_SERVER_ID --out /data --download-attachments >>/var/log/discord-backup.log 2>&1
```

## MCP (read-only access)

The same image serves the MCP endpoint; just pass the subcommand:

```bash
docker run --rm -i \
  -v /var/lib/discord-backup:/data \
  -v /etc/discord-backup/token:/token:ro \
  -v /etc/discord-backup/mcp.json:/etc/mcp.json:ro \
  discord-backup mcp --config /etc/mcp.json --principal martin
```

`-i` keeps stdin open (stdio transport). For HTTP, publish the port and keep
the config's `archive_dir` pointing at `/data`:

```bash
docker run --rm -p 127.0.0.1:8765:8765 \
  -v /var/lib/discord-backup:/data \
  -v /etc/discord-backup/mcp.json:/etc/mcp.json:ro \
  discord-backup mcp --config /etc/mcp.json --http --addr 0.0.0.0:8765
```

Serve the archive **read-only** (`-v ...:/data:ro`) if the MCP runs in a
separate container from the backup. See [mcp.md](mcp.md).

### MCP as a service (Compose)

The HTTP transport is a normal long-running process, so it can be a service
with `restart: unless-stopped`. A ready-to-copy template lives in
[`compose.example.yaml`](../compose.example.yaml):

```bash
cp compose.example.yaml compose.yaml
cp mcp.example.json mcp.json     # set "archive_dir": "/data" and a token
printf '%s' "$BOT_TOKEN" > token && chmod 600 token
mkdir -p archive
sudo chown 65532:65532 archive token   # the container runs as 65532:65532
docker compose up -d mcp
```

The template:

```yaml
services:
  mcp:
    image: ghcr.io/martinverges/discord-backup:latest
    restart: unless-stopped
    environment:
      DISCORD_TOKEN_FILE: /token          # or set "token_file" in mcp.json
    volumes:
      - ./archive:/data:ro            # read-only: the MCP never writes
      - ./mcp.json:/etc/mcp.json:ro
      - ./token:/token:ro
    command: ["mcp", "--config", "/etc/mcp.json", "--http", "--addr", "0.0.0.0:8765"]
    ports:
      - "127.0.0.1:8765:8765"         # keep local; add a TLS proxy to expose
    healthcheck:
      test: ["CMD", "/usr/local/bin/discord-backup", "healthcheck", "--url", "http://127.0.0.1:8765/healthz"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
```

Points that matter:

- **Healthcheck without a shell.** The distroless image ships no shell, `curl`
  or `wget`, so the healthcheck execs the binary itself:
  `discord-backup healthcheck --url http://127.0.0.1:8765/healthz` exits `0`
  exactly when `/healthz` answers `200`.
- **Share one archive.** The backup writes `./archive`; the MCP mounts the same
  directory (`:ro`). Its `mcp.json` must use the container path:
  `"archive_dir": "/data"`.
- **Token permissions.** The token file must be mode `0600` (the server refuses
  anything group/world-readable) *and* readable by uid `65532`, hence the
  `chown`. Point the server at it either via `"token_file": "/token"` in
  `mcp.json` or via `DISCORD_TOKEN_FILE` (as in the template). Docker/Compose
  *secrets* are mounted `0444` by default and would be rejected — a
  bind-mounted file is simpler here.
- **Backup and MCP in parallel is fine.** The archive is append-only and the
  reader detects rewrites; a backup running while clients query causes no
  inconsistency. See [mcp.md](mcp.md).
- **Exposure.** `127.0.0.1:8765` keeps it local. To expose it, terminate TLS in
  a reverse proxy and keep the per-principal bearer tokens; there is no TLS or
  additional auth in the server itself.

## Notes

- HTTPS to Discord works out of the box (distroless ships CA certificates).
- The lock file lives at `/data/.lock` – overlapping `docker run` calls exit cleanly like the binary does.
- No healthcheck needed: the container exits after each sync (exit `0` = ok).
