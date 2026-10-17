# discord-backup

Incrementally backs up a Discord server to local files. Each invocation syncs everything since the last run – driven by cron, a systemd timer, or its own built-in interval. One [configuration file](config.example.json) drives everything, backup and MCP alike.

- **Go, standard library only:** a single binary, no dependencies, runs on Linux, macOS and Windows
- **No duplicates:** the data file itself is the cursor, no separate state to drift
- **Complete:** channels, threads (active + archived), pins, attachments, edits and deletes
- **Secure:** one `0600` config file holds the token (no env, no CLI argument, no second file), only Discord's CDN is fetched, filenames are sanitized
- **Queryable:** optional read-only [MCP server](docs/mcp.md) that only shows each user what their Discord permissions allow

```
Channels · Threads · Pins · Attachments → JSONL files → schedule → done
                                                       ↘ MCP (permission-scoped)
```

## Quick start

```bash
go build -o discord-backup .

# 1. Set up the bot (one time, ~5 min) → docs/bot-setup.md
# 2. Configure: copy the example and set guild_id, archive_dir, token
cp config.example.json config.json
chmod 600 config.json

# 3. Dry run: show what would be archived
./discord-backup --list-channels

# 4. Back up
./discord-backup

# 5. Automate: cron/timer, or keep it running (set "backup": {"every": "15m"})
#    → docs/automation.md
```

## Documentation

| Page | Contents |
|---|---|
| [docs/installation.md](docs/installation.md) | Building and installing |
| [docs/bot-setup.md](docs/bot-setup.md) | Creating, inviting and permitting the Discord bot |
| [docs/usage.md](docs/usage.md) | The configuration file, command-line options, examples |
| [docs/automation.md](docs/automation.md) | Built-in service mode, cron, systemd timers, launchd |
| [docs/archive-format.md](docs/archive-format.md) | File format and querying with `jq` |
| [docs/mcp.md](docs/mcp.md) | Read-only MCP access, scoped by Discord permissions |
| [docs/docker.md](docs/docker.md) | Minimal container image, backup + MCP as Compose services |
| [docs/troubleshooting.md](docs/troubleshooting.md) | Errors and fixes |

## Example

```bash
$ ./discord-backup --config config.json
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
# Same config file as the backup; describe who may connect under "mcp":
#   "mcp": { "principals": [
#     { "name": "martin", "discord_id": "123456789012345678" } ] }

# Build that list from the guild member list instead of copying IDs by hand
# (prints a ready-to-paste block; --write merges it into the config):
./discord-backup principals --config config.json

# stdio for Claude Desktop/Code, --http for remote
./discord-backup mcp --config config.json
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
chmod 600 config.json
sudo chown 65532:65532 config.json archive   # the image is not root
docker run --rm -v "$PWD/archive:/data" \
  -v "$PWD/config.json:/etc/discord-backup.json:ro" \
  ghcr.io/martinverges/discord-backup:latest --config /etc/discord-backup.json
```

Details: [docs/docker.md](docs/docker.md). Every version tag publishes an image next to it.

## Releases

Versions are **`YYMM.x`** – two digits of year, two of month, a dot, then the
release number within that month:

| Tag | Meaning |
|---|---|
| `2610.1` | first release of October 2026 |
| `2610.2` | second release of October 2026 |
| `2611.1` | first release of November 2026 |

One command cuts the next one (it only prints the tag unless you say `--yes`):

```bash
./scripts/release.sh          # 2610.1   (nothing changed)
./scripts/release.sh --yes    # tag + push -> Release workflow builds the binaries,
                              # Docker workflow publishes ghcr.io/…:2610.1
```

Everything else follows the tag: the archive names
(`discord-backup_2610.1_linux_amd64.tar.gz`), `discord-backup --version`, the
version MCP clients see in `initialize`, and the container image tag. The same
number is never released twice – a fix gets the next number of its month.

## License

MIT License – Copyright (c) 2026 Martin Verges. See [LICENSE](LICENSE).
