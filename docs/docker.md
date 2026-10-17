# Docker

Prebuilt images (amd64 + arm64), published by the Docker workflow:

- `ghcr.io/martinverges/discord-backup:latest` – built from every `main` commit
- `ghcr.io/martinverges/discord-backup:v0.1.0` – built from the `v0.1.0` tag

(Binaries for Linux/macOS/Windows live on the Releases page; the Release
workflow builds those, images are published separately.)

No release yet or want to build yourself? Minimal image (~16 MB): static Go
binary on distroless (Debian 13), running as non-root (uid 65532).

Everything the container needs comes from **one config file** (see
[usage.md](usage.md)); there is no token file and no environment variable. A
`docker run` is therefore just: mount the config, mount the archive, pass
`--config`.

## Build

```bash
docker build -t discord-backup .
```

## Run (one pass)

```bash
cp config.example.json config.json   # set guild_id, archive_dir, token
chmod 600 config.json                # it holds the token
mkdir -p archive
# bind mounts must be readable/writable for uid 65532 (or use --user, below)
sudo chown 65532:65532 archive config.json

docker run --rm \
  -v "$PWD/archive:/data" \
  -v "$PWD/config.json:/etc/discord-backup.json:ro" \
  discord-backup --config /etc/discord-backup.json
```

`archive_dir` in the config must be the **container** path (`/data` here).

Alternative without chown – run as your own uid:

```bash
docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD/archive:/data" \
  -v "$PWD/config.json:/etc/discord-backup.json:ro" \
  discord-backup --config /etc/discord-backup.json
```

## Cron (host)

The host schedules a one-shot container; `backup.every` stays empty in the
config:

```
*/15 * * * * /usr/bin/docker run --rm -v /var/lib/discord-backup:/data -v /etc/discord-backup.json:/etc/discord-backup.json:ro discord-backup --config /etc/discord-backup.json >>/var/log/discord-backup.log 2>&1
```

## Service (no cron)

Set `"every": "15m"` under `backup` and the container becomes its own
scheduler: it backs up, waits 15 minutes **after** each pass, then runs again.
Compose has no scheduler of its own, so this is the way to keep everything in
one compose file:

```yaml
services:
  backup:
    image: ghcr.io/martinverges/discord-backup:latest
    restart: unless-stopped          # comes back after reboot/OOM
    volumes:
      - ./archive:/data
      - ./config.json:/etc/discord-backup.json:ro
    command: ["--config", "/etc/discord-backup.json"]
```

Or as a one-liner:

```bash
docker run -d --restart unless-stopped --name discord-backup \
  -v "$PWD/archive:/data" \
  -v "$PWD/config.json:/etc/discord-backup.json:ro" \
  ghcr.io/martinverges/discord-backup:latest --config /etc/discord-backup.json
```

Details:

- The interval is measured **after** each pass finishes, so passes never overlap
  and a run that takes 10 minutes with `"every": "15m"` simply starts 15
  minutes after it ends (`docker logs discord-backup` logs `every=15m` at
  start).
- A failing pass does **not** stop the loop: it is logged and retried on the
  next tick, so a Discord outage or a temporary network error costs one
  interval, not the schedule.
- `SIGINT`/`SIGTERM` (e.g. `docker stop`) ends the loop cleanly with exit `0`.
- Still safe next to a second instance or a host cron: the lock in
  `<archive_dir>/.lock` makes an overlapping run exit `0` with
  `another backup run is active, exiting`.

## MCP (read-only access)

The same image – and the same config file – serves the MCP endpoint; just pass
the subcommand:

```bash
docker run --rm -i \
  -v /var/lib/discord-backup:/data:ro \
  -v /etc/discord-backup.json:/etc/discord-backup.json:ro \
  discord-backup mcp --config /etc/discord-backup.json --principal martin
```

`-i` keeps stdin open (stdio transport). For HTTP, publish the port; the listen
address is `mcp.http_addr` in the config:

```bash
docker run --rm -p 127.0.0.1:8765:8765 \
  -v /var/lib/discord-backup:/data:ro \
  -v /etc/discord-backup.json:/etc/discord-backup.json:ro \
  discord-backup mcp --config /etc/discord-backup.json --http
```

The archive is mounted **read-only** (`:ro`) because the MCP never writes to
it. See [mcp.md](mcp.md).

### Backup + MCP as services (Compose)

Both parts are long-running processes with `restart: unless-stopped`: the
backup schedules itself (`backup.every`), the MCP serves HTTP. A ready-to-copy
template lives in [`compose.example.yaml`](../compose.example.yaml):

```bash
cp compose.example.yaml compose.yaml
cp config.example.json config.json    # set archive_dir "/data", token, every, mcp
chmod 600 config.json                 # it holds tokens
mkdir -p archive
sudo chown 65532:65532 archive config.json   # container runs as 65532:65532
docker compose up -d                  # starts backup and mcp
```

The template mounts the same config into both services:

```yaml
services:
  backup:
    image: ghcr.io/martinverges/discord-backup:latest
    restart: unless-stopped
    volumes:
      - ./archive:/data
      - ./config.json:/etc/discord-backup.json:ro
    command: ["--config", "/etc/discord-backup.json"]

  mcp:
    image: ghcr.io/martinverges/discord-backup:latest
    restart: unless-stopped
    volumes:
      - ./archive:/data:ro            # read-only: the MCP never writes
      - ./config.json:/etc/discord-backup.json:ro
    command: ["mcp", "--config", "/etc/discord-backup.json", "--http"]
    ports:
      - "127.0.0.1:8765:8765"         # keep local; add a TLS proxy to expose
    healthcheck:
      test: ["CMD", "/usr/local/bin/discord-backup", "healthcheck", "--config", "/etc/discord-backup.json"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
```

Points that matter:

- **Healthcheck without a shell.** The distroless image ships no shell, `curl`
  or `wget`, so the healthcheck execs the binary itself:
  `discord-backup healthcheck --config /etc/discord-backup.json` probes
  `http://<mcp.http_addr>/healthz` and exits `0` exactly when it answers `200`.
  Taking the address from the config means it cannot drift from what the server
  binds, and a broken config is reported as unhealthy instead of unnoticed.
- **One config, one secret rule.** No environment variables, no token file: the
  bot token and the principals' bearer tokens all sit in `config.json`. Because
  it holds tokens it must be mode `0600` **and** owned by the uid the process
  runs as (see below). Docker/Compose *secrets* are mounted `0444` by default
  and would be rejected; a bind-mounted file is simpler here.
- **Only your own section is enforced.** The backup validates `backup.*`, the
  MCP validates `mcp.*`; an unfinished `mcp` section cannot stop the backup.
  (Malformed values – a bad duration, a negative number – are document errors
  and always fail.)
- **Share one archive.** The backup writes `./archive`; the MCP mounts the same
  directory (`:ro`). `archive_dir` must be the container path `/data` for both.
- **`audit_log` needs a writable path.** The MCP's archive mount is read-only,
  so an `audit_log` inside it fails; mount a second volume (or use `/tmp`) and
  point `mcp.audit_log` at it.
- **Backup and MCP in parallel is fine.** The archive is append-only and the
  reader detects rewrites; a backup running while clients query causes no
  inconsistency. See [mcp.md](mcp.md).
- **Exposure.** `127.0.0.1:8765` keeps it local. To expose it, terminate TLS in
  a reverse proxy and keep the per-principal bearer tokens; there is no TLS or
  additional auth in the server itself.

### Permissions of `config.json` (the usual first failure)

The image runs as uid/gid `65532` (`nonroot`), **not** as root, so the bind mount
must be readable by that user. The two errors that follow from this are:

| Message | Cause | Fix |
|---|---|---|
| `… holds tokens and must not be group/world accessible` | mode is `0644`/`0664` | `chmod 600 config.json` |
| `read /etc/discord-backup.json: permission denied as uid 65532` | mode is fine, but the file belongs to your login | `sudo chown 65532:65532 config.json` |

`chmod 600` alone is not enough – it removes exactly the access the container was
using. Do both, plus the archive the backup writes to:

```bash
chmod 600 config.json
sudo chown 65532:65532 config.json
sudo chown -R 65532:65532 archive
```

**Alternative:** run the containers as your own user and keep the file owned by
you:

```yaml
services:
  backup:
    user: "1000:1000"     # id -u:id -g
  mcp:
    user: "1000:1000"     # the same uid as the backup
```

`chmod 600` still applies, and `archive/` must belong to that uid too.

After the container started once, the file stays owned by 65532 – if you edit it
in place, ownership and mode are preserved, so no repeat is needed. If a tool
rewrote it (`cp` from somewhere else, rsync, a UI editor that recreates files),
re-run the `chmod`/`chown`.

## Notes

- HTTPS to Discord works out of the box (distroless ships CA certificates).
- The lock file lives at `<archive_dir>/.lock` – overlapping `docker run` calls exit cleanly like the binary does.
- A running container reads `config.json` **once at start**: changing it needs a
  restart (`docker compose up -d --force-recreate backup`).
- No healthcheck needed for the **backup** container: in service mode it is alive as long as the loop runs, and a failing pass only logs; the **MCP** service has one, above.
