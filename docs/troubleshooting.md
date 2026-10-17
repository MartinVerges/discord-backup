# Troubleshooting

## First run is slow / seems stuck on a channel

Normal for large channels: the first run pages the complete history at
Discord's maximum of 100 messages per request (there is no bigger page size
and no jump-to-oldest in the API), sequentially and rate-limit aware. Watch
for `fetching messages … phase=backfill` heartbeats (every 15 s) and
`fetch complete` with page/message counts. Chunks are written to disk as they
arrive, so Ctrl-C is safe: the next run resumes (newer messages via catch-up,
older ones via heal) instead of starting over. To skip old history entirely,
set `"since"` under `backup` (scans forward from that date, no paging
through older history).

Forums are the long pole: every post is a thread with its own log, so a forum
channel can take far longer than any text channel. The log says so -
`archived threads scanned threads=… pages=…` before the posts,
`thread backup progress done=… of=…` every 15 s while working through them,
and the per-channel `threads=N` in `channel done`. If none of these lines
appear for minutes, the job really is stuck rather than busy.

Two loops used to be able to spin forever, producing exactly that silence -
no output, no writes, only Discord requests:

* An archived-thread page that Discord hands back again (an inclusive or
  coarser `before`) was re-fetched unchanged. The cursor now steps one second
  back once and then stops with
  `archived threads: cursor stopped advancing, stopping`.
* A page consisting only of filtered-out (bot) messages did not move the
  message cursor. The cursor now follows the page, not the stored messages.

Both loops are guarded now, so they stop with a warning instead of spinning.
If the log still stops for minutes, the run is waiting on a request rather
than busy: the last line names the channel, the container can be restarted
safely (archives are append-only and resume), and the same channel is picked
up again next run. To see whether a run is progressing right now, watch the
archive on the host:

```bash
# files written in the last 3 minutes (busy) vs. nothing (stuck)
find "./archive" -newermt '-3 minutes' | head
```

## `forbidden … Missing Access (code 50001)`

The bot may not read the channel. Server Settings → Roles → bot role: enable **View Channels**
and **Read Message History**. For restricted channels, explicitly grant the bot role in the
channel (or the category, which inherits down). Details: [bot-setup.md](bot-setup.md).

## Messages have empty `content`

Enable the **Message Content Intent** in the Developer Portal (Bot → Privileged Gateway Intents → Save Changes).

## `not found … threads/active` / `active threads unavailable`

Older versions used the per-channel endpoint, which answers 404 on API v10. Current versions use
the guild endpoint – just rebuild (`go build`).

## Token errors

- `no bot token: set "token" in config.json`: add the token to the config.
- `config … holds tokens and must not be group/world accessible`: `chmod 600 config.json`.
- `401 Unauthorized`: token wrong/expired → regenerate via **Reset Token** in the portal.

## `another backup run is active`

Not an error: another process holds the archive (a second container, a cron run,
a manual run). A one-shot run exits `0`; a service (`backup.every`) waits at most
a minute and retries, so it picks the work up by itself. If it never stops:

```bash
sudo lsof "/path/to/archive/.lock"      # which process holds it
docker ps --format '{{.Names}}\t{{.Status}}' | grep -i backup
```

The pid in the message is the holder's pid *inside its own namespace* – `1`
usually means "another container". A stale lock file needs no cleanup (it is an
OS lock: a dead process releases it), but deleting `<archive_dir>/.lock` is
harmless if you want to be sure.

## MCP: port published, but nothing answers

Almost always the container's loopback: `mcp.http_addr` is what the process
listens on **inside** the container, and a published port cannot reach
`127.0.0.1` there. The container's own healthcheck still passes, which makes it
look healthy.

```json
{ "mcp": { "http_addr": "0.0.0.0:8765" } }
```

Keep the **host** side loopback in compose (`"127.0.0.1:8765:8765"`). The server
logs a warning when it binds a loopback address inside a container:
[Host vs. container address](docker.md#host-vs-container-address).

Two quick ways to isolate it – the first one works even when the port is not
published at all, because it runs the probe inside the container:

```bash
docker compose exec mcp /usr/local/bin/discord-backup \
  healthcheck --config /etc/discord-backup.json --principal auto

curl -sS -X POST http://127.0.0.1:8765/mcp \
  -H 'Content-Type: application/json' -H 'Authorization: Bearer <principal token>' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

`401` from the second one means the port works and only the token is wrong; a
connection error means nothing is listening on the published port.

## Config errors

- `mcp.http_addr is set but no principal has a "token"`: the HTTP transport
  authenticates every caller with their own bearer token, so each principal that
  may connect needs one (`discord-backup principals --config config.json`
  generates them, `openssl rand -hex 32` works too). Only the MCP minds this –
  the backup starts anyway, because it never touches the `mcp` section.
- `no principals configured: add "mcp.principals"`: the MCP needs at least one
  identity mapping (name/email/alias → `discord_id`). `discord-backup principals
  --config config.json` builds it from the guild's member list.
- `Discord refused the member list (403)` (from `principals`): the bot lacks the
  privileged **Server Members Intent** – Developer Portal → your app → Bot →
  Privileged Gateway Intents.
- `unknown field "..."`: a typo – unknown keys are rejected on purpose, so a
  misspelled setting fails loudly instead of being ignored.
- `config … holds tokens and must not be group/world accessible`: `chmod 600 config.json`.
- `read /etc/discord-backup.json: permission denied as uid 65532`: the mode is
  fine, but the file belongs to another user – the container does not run as
  root. `sudo chown 65532:65532 config.json` (or run the container with
  `user: "1000:1000"`). Both messages and their fixes are in
  [docker.md](docker.md#permissions-of-configjson-the-usual-first-failure).
- Edits to `config.json` take effect only after a restart (the file is read once
  at start-up): `docker compose up -d --force-recreate backup`, or restart the
  systemd unit/cron never needs it.

## Rate limits / slow first run

Normal: Discord throttles requests, the tool complies and patiently backfills history.
Interrupting is safe – the next run continues. For very active servers, leave
`download_attachments` off on the first run and catch up later.

## `attachments_failed > 0`

Individual downloads (expired CDN links, files over `attach_max_mb`) may fail while the run
continues. Missing files are retried on the next run as long as the URL is still valid.
