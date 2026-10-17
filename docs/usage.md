# Usage

```
discord-backup [--config config.json] [--list-channels]
discord-backup mcp --config config.json [--principal NAME | --http]
discord-backup principals --config config.json [--write]
```

`discord-backup mcp` serves the archive read-only over MCP, limited to each
caller's Discord permissions – see [mcp.md](mcp.md).

`discord-backup principals` turns the guild's member list into `mcp.principals`
entries (the name people use on the server, the real `discord_id`, a fresh token
each) and prints a ready-to-paste block; `--from-channel` keeps only the members
who may read that channel, `--write` merges the result into the config. See
[mcp.md](mcp.md#building-the-list-from-discord).

## One configuration file

**Everything is configured in one file** – the bot token, the guild, the
archive directory, the backup schedule and the MCP settings. The backup and the
MCP server read the *same* file, so there is no token file, no separate MCP
config and no environment variable. `--config` defaults to `config.json`.

```json
{
  "guild_id": "123456789012345678",
  "archive_dir": "/var/lib/discord-backup",
  "token": "your_bot_token",

  "backup": {
    "download_attachments": true,
    "skip_empty": true,
    "edit_window": "48h",
    "every": "15m"
  },

  "mcp": {
    "http_addr": "127.0.0.1:8765",
    "principals": [
      { "name": "martin", "discord_id": "111111111111111111", "token": "random-http-token" }
    ]
  }
}
```

Start from [`config.example.json`](../config.example.json) and then:

```bash
cp config.example.json config.json
chmod 600 config.json       # required: the file holds the token
discord-backup              # one backup pass, ./config.json
```

A config that contains a token (the bot token or any principal token) **must be
mode `0600`**; the tool refuses to start otherwise. It must also be readable by
the user the process runs as – in a container that is usually the trap, see
[docker.md](docker.md#permissions-of-configjson-the-usual-first-failure).
Unknown keys are rejected, so a typo is an error instead of a silently ignored
setting.

### Top level

| Key | Meaning |
|---|---|
| `guild_id` | Discord server ID, **required** |
| `archive_dir` | Archive root, **required** |
| `token` | Bot token. Required for the backup; without it the MCP runs snapshot-only |

### `backup`

| Key | Default | Meaning |
|---|---|---|
| `download_attachments` | `false` | Store attachments locally |
| `backfill_attachments` | `false` | Download attachments missing on disk for already archived messages (message data untouched) |
| `include_bots` | `false` | Also archive bot and webhook messages |
| `skip_empty` | `true` | Skip channels with no access or zero messages |
| `edit_window` | `"48h"` | Window for edit/delete detection (`"0"` = new messages only) |
| `attach_max_mb` | `50` | Max attachment size in MB; `0` means the default `50` (there is no "unlimited") |
| `attach_workers` | `4` | Parallel attachment downloads |
| `every` | `""` (once) | Service mode: wait this long **after** each pass, then back up again |
| `limit` | `0` (= unlimited) | Initial backfill cap per channel/thread, newest N (first run only); mutually exclusive with `since` |
| `since` | `""` | Only archive messages newer than this date (`YYYY-MM-DD` or RFC3339), first run only |
| `channels` | `[]` | Restrict the archive to these channels/threads (name or ID) |

### `mcp`

| Key | Default | Meaning |
|---|---|---|
| `http_addr` | `127.0.0.1:8765` | Listen address for `mcp --http` |
| `default_principal` | – | Identity for stdio when `--principal` is omitted |
| `deleted` | `"hide"` | `hide` deleted messages, or `show` them (marked `deleted`) |
| `refresh_ttl` | `"60s"` | How often the archive is rescanned for new data |
| `channel_ttl` | `"5m"` | Cache lifetime for channel overwrites + roles |
| `member_ttl` | `"60s"` | Cache lifetime for one member's roles |
| `retry_ttl` | `"5m"` | Pause live lookups this long after a failure, then retry |
| `timeout` | `"5s"` | Bound per live lookup, so an outage degrades quickly |
| `check_private_threads` | `false` | Verify private-thread membership live (extra API calls) |
| `audit_log` | – | Optional JSONL file, one line per tool call |
| `principals` | – | Identity mapping; see [mcp.md](mcp.md#principals) |

Durations like `"48h"`, `"24h"`, `"30m"`, `"0"` are accepted.

## Command-line options

Since every setting lives in the config, the CLI is small:

| Option | Meaning |
|---|---|
| `--config PATH` | Configuration file (default `config.json`) |
| `--list-channels` | Show channels as a table and exit (backup) |
| `--principal NAME` | Identity for the stdio transport (mcp) |
| `--http` | Serve HTTP instead of stdio (mcp) |
| `--write` | Merge the generated principals into the config (principals) |
| `--from-channel CH` | Keep only members who may read CH (principals) |
| `--keep-names` | Leave existing principal names alone (principals) |
| `--include-bots` | Keep bot accounts in the list (principals) |
| `--no-token` | Generate principals without tokens, for stdio (principals) |
| `--quiet` | Only log warnings and errors |
| `--version` | Print version and exit |

Plus the `healthcheck` subcommand, which needs no config of its own:

```
discord-backup healthcheck --config /etc/discord-backup.json   # probes mcp.http_addr
discord-backup healthcheck --url http://127.0.0.1:8765/healthz
```

With `--principal` it does a real, read-only MCP round trip instead of a bare
ping - `initialize`, `tools/list` and `whoami` with that principal's bearer
token - which is the quickest way to check auth, archive and permissions:

```
discord-backup healthcheck --config /etc/discord-backup.json --principal auto
```

```
http://127.0.0.1:8765/mcp ok: discord-backup 0.2.0 (protocol 2025-06-18)
  tools (7): whoami, list_channels, list_threads, search_messages, get_message, get_context, list_authors
  authenticated as martin (111111111111111111)
  visible: 71 channels, 6012 threads, 310145 messages (permissions: live)
```

`auto` picks any principal that has a `token`; a name, email, alias or
`discord_id` picks a specific one. Exit code `0` only when the call succeeded.

### Which section is enforced when

Validation is split so one command cannot be blocked by the other's section:
the backup enforces `backup.*` only (e.g. `limit` with `since` is rejected), the
MCP enforces `mcp.*` only (at least one principal, `deleted`, and a principal
token whenever `http_addr` is set). Malformed values – an unknown key, a bad
duration, a negative number, a missing `guild_id`/`archive_dir` – are document
errors and stop both.

The config is read **once at start-up**; after editing it, restart the process
(or container) for the change to take effect.

## Examples

```bash
# One pass, using ./config.json
discord-backup

# Somewhere else, with --list-channels
discord-backup --config /etc/discord-backup.json --list-channels

# Quiet in cron (only check the log on problems)
discord-backup --config /etc/discord-backup.json --quiet

# Read-only MCP server
discord-backup mcp --config config.json --principal martin
```

Trial runs and one-offs are config edits, not flags – e.g.

```json
{ "backup": { "channels": ["general"], "limit": 100, "edit_window": "0" } }
```

```json
{ "backup": { "since": "2026-01-01" } }
```

```json
{ "backup": { "backfill_attachments": true } }
```

```json
{ "backup": { "include_bots": true } }
```

## Scheduling

Two equivalent ways to keep an archive fresh:

- **Cron / systemd timer** on the host: one process per tick, exit when done.
- **Service mode** (`backup.every`): one long-running process that backs up,
  waits `every` after each pass, then runs again. Passes never overlap, a
  failing pass is retried on the next tick instead of stopping the loop, and
  `SIGINT`/`SIGTERM` shuts down cleanly (exit `0`). This is what the Docker
  Compose template uses, since Compose has no scheduler of its own.

Either way, runs that do overlap are safe: the lock in `<archive_dir>/.lock`
(a real OS lock, `flock`) makes a second run stop – a one-shot run exits `0`
with `another backup run is active, exiting`, a service waits and re-checks
after at most a minute (`min(every, 1m)`) instead of sitting idle for the whole
interval. A crashed process never leaves the lock behind; the OS drops it.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | OK – including when another run is currently active (lock, not an error) |
| `1` | Error (config, token, network, guild-level permissions, …) |
| `130` | Interrupted via Ctrl-C/signal |

Individual problem channels (no access, deleted) never abort the run – they are logged, skipped, and marked `no_access` in the index.
