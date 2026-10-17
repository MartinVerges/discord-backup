# discord-backup

Incrementally backs up a Discord server to local files. Each invocation syncs everything since the last run – driven by cron, a systemd timer, or its own `--every` loop.

- **Go, standard library only:** a single binary, no dependencies, runs on Linux, macOS and Windows
- **No duplicates:** the data file itself is the cursor, no separate state to drift
- **Complete:** channels, threads (active + archived), pins, attachments, edits and deletes
- **Secure:** token never passed as an argument, only Discord's CDN is fetched, filenames are sanitized
- **Queryable:** optional read-only [MCP server](docs/mcp.md) that only shows each user what their Discord permissions allow

```
Channels · Threads · Pins · Attachments → JSONL files → cron → done
                                                       ↘ MCP (permission-scoped)
```

## Quick start

```bash
go build -o discord-backup .

# 1. Set up the bot (one time, ~5 min) → docs/bot-setup.md
# 2. Dry run: show what would be archived
./discord-backup --guild YOUR_SERVER_ID --token-file ~/.discord-backup-token --list-channels

# 3. Back up
./discord-backup --guild YOUR_SERVER_ID --out ./discord_archive --download-attachments --token-file ~/.discord-backup-token

# 4. Automate: cron/timer, or stay in the foreground → docs/automation.md
./discord-backup --guild YOUR_SERVER_ID --out ./discord_archive --download-attachments --token-file ~/.discord-backup-token --every 15m
```

## Documentation

| Page | Contents |
|---|---|
| [docs/installation.md](docs/installation.md) | Building and installing |
| [docs/bot-setup.md](docs/bot-setup.md) | Creating, inviting and permitting the Discord bot |
| [docs/usage.md](docs/usage.md) | All options, environment variables, examples |
| [docs/automation.md](docs/automation.md) | `--every` service mode, cron, systemd timers, launchd |
| [docs/archive-format.md](docs/archive-format.md) | File format and querying with `jq` |
| [docs/mcp.md](docs/mcp.md) | Read-only MCP access, scoped by Discord permissions |
| [docs/docker.md](docs/docker.md) | Minimal container image, service mode (`--every`) |
| [docs/troubleshooting.md](docs/troubleshooting.md) | Errors and fixes |

## Example

```bash
$ ./discord-backup --guild 123456789012345678 --out archive/ --download-attachments
time=... level=INFO msg=authenticated bot="Backup Bot"
time=... level=INFO msg=guild name=example-guild humans=68 approx_total=74
time=... level=INFO msg="active threads" count=231
time=... level=INFO msg="channel done" channel=#random new=0 edits=0 deleted=0 threads=10 pinned=5 status=ok
time=... level=INFO msg=done channels=75 attachments_new=12 attachments_cached=340 attachments_failed=0
```

## MCP access (read-only, permission-scoped)

Serve the archive to an LLM or any MCP client, limited to what the caller may
read in Discord:

```bash
# 1. Describe who may connect (name / email / discord_id → Discord user)
cat > mcp.json <<'JSON'
{
  "archive_dir": "/var/lib/discord-backup",
  "token_file": "/etc/discord-backup/token",
  "principals": [
    { "name": "martin", "discord_id": "123456789012345678" }
  ]
}
JSON

# 2. Run (stdio for Claude Desktop/Code, --http for remote)
./discord-backup mcp --config mcp.json
```

Tools: `whoami`, `list_channels`, `list_threads`, `search_messages`,
`get_message`, `get_context`, `list_authors`. Access is computed from Discord's
permissions, live with the archive's `permissions.json` as offline fallback.
Details: [docs/mcp.md](docs/mcp.md). To run it as a long-lived HTTP service in
Docker (with healthcheck), see [docs/docker.md](docs/docker.md) and
[`compose.example.yaml`](compose.example.yaml).

## Docker

```bash
docker pull ghcr.io/martinverges/discord-backup:latest
docker run --rm --env-file .env -v "$PWD/archive:/data" \
  ghcr.io/martinverges/discord-backup:latest --guild YOUR_SERVER_ID --out /data --download-attachments
```

Details: [docs/docker.md](docs/docker.md). A new image is published automatically with every `v*` release.

## License

MIT License – Copyright (c) 2026 Martin Verges. See [LICENSE](LICENSE).
