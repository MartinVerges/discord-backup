# Usage

```
discord-backup --guild GUILD_ID [options]
discord-backup mcp --config mcp.json [--principal NAME | --http]
```

`discord-backup mcp` serves the archive read-only over MCP, limited to each
caller's Discord permissions – see [mcp.md](mcp.md).

## Token (pick one)

| Source | Example |
|---|---|
| File (recommended) | `--token-file ~/.discord-backup-token` or `DISCORD_TOKEN_FILE=...` |
| Environment variable | `DISCORD_TOKEN=...` |

The file must have mode `0600` (`chmod 600`), otherwise the tool refuses to start.

## Options

| Option | Default | Meaning |
|---|---|---|
| `--guild ID` | `DISCORD_GUILD_ID` | Server ID (required) |
| `--out DIR` | `ARCHIVE_PATH` / `./discord_archive` | Destination directory |
| `--channel NAME/ID` | – | Only archive these channel(s) (repeatable) |
| `--download-attachments` | off | Store attachments locally |
| `--backfill-attachments` | off | Download attachments missing on disk for already archived messages (implies download; message data untouched) |
| `--include-bots` | off | Also archive bot and webhook messages |
| `--limit N` | `0` (= unlimited) | Limit the initial backfill to the newest N messages per channel/thread (first run only; existing archives are never truncated) |
| `--since DATE` | – | Only archive messages newer than this date (`YYYY-MM-DD` or RFC3339), first run only; mutually exclusive with `--limit` |
| `--edit-window DURATION` | `48h` | Time window for edit/delete detection (`0` = new messages only) |
| `--attach-max-mb N` | `50` | Max attachment size in MB |
| `--attach-workers N` | `4` | Parallel attachment downloads |
| `--skip-empty` | on | Skip channels with no access or zero messages |
| `--list-channels` | off | Show channels as a table and exit |
| `--version` | off | Print version and exit |
| `--quiet` | off | Only log warnings and errors |

Durations like `48h`, `24h`, `30m`, `0` are accepted.

## Examples

```bash
# Archive everything, with attachments
discord-backup --guild 123 --out /var/lib/discord-backup --download-attachments

# Only two channels
discord-backup --guild 123 --channel general --channel 987654321

# Trial run: 100 newest messages from one channel, no edit reconciliation
discord-backup --guild 123 --channel general --limit 100 --edit-window 0

# Only archive messages from 2026 on (first run scans forward from that date)
discord-backup --guild 123 --since 2026-01-01

# Fetch attachments missed earlier (e.g. old size cap, or archived before --download-attachments)
discord-backup --guild 123 --backfill-attachments

# Include bots (backfilled retroactively, without duplicates)
discord-backup --guild 123 --include-bots

# Quiet in cron (only check the log on problems)
discord-backup --guild 123 --quiet
```

## Exit codes

| Code | Meaning |
|---|---|
| `0` | OK – including when another run is currently active (lock, not an error) |
| `1` | Error (token, network, guild-level permissions, …) |
| `130` | Interrupted via Ctrl-C/signal |

Individual problem channels (no access, deleted) never abort the run – they are logged, skipped, and marked `no_access` in the index.
