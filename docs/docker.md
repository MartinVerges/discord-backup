# Docker

Prebuilt image (amd64 + arm64), published automatically with every release:

```bash
docker pull ghcr.io/martinverges/discord-backup:latest

docker run --rm \
  --env-file .env \
  -v "$PWD/archive:/data" \
  ghcr.io/martinverges/discord-backup:latest --guild YOUR_SERVER_ID --out /data --download-attachments
```

No release yet or want to build yourself? Minimal image (~12 MB): static Go binary on
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

## Notes

- HTTPS to Discord works out of the box (distroless ships CA certificates).
- The lock file lives at `/data/.lock` – overlapping `docker run` calls exit cleanly like the binary does.
- No healthcheck needed: the container exits after each sync (exit `0` = ok).
