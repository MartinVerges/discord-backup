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

## `forbidden … Missing Access (code 50001)`

The bot may not read the channel. Server Settings → Roles → bot role: enable **View Channels**
and **Read Message History**. For restricted channels, explicitly grant the bot role in the
channel (or the category, which inherits down). Details: [bot-setup.md](bot-setup.md).

## Messages have empty `content`

Enable the **Message Content Intent** in the Developer Portal (Bot → Privileged Gateway Intents → Save Changes).

## `not found … threads/active` / `active threads unavailable`

Older versions used the per-channel endpoint, which answers 404 on API v10. Current versions use
the guild endpoint – just rebuild (`go build`).

## `another backup run is active`

Not an error: the previous run is still working. The cron run exits cleanly and the next one
catches up. If the message persists, a process is stuck (`ps aux | grep discord-backup`).

## Token errors

- `no bot token: set "token" in config.json`: add the token to the config.
- `config … holds tokens and must not be group/world accessible`: `chmod 600 config.json`.
- `401 Unauthorized`: token wrong/expired → regenerate via **Reset Token** in the portal.

## Config errors

- `mcp.http_addr is set but no principal has a "token"`: the HTTP transport
  authenticates every caller with their own bearer token, so each principal that
  may connect needs one (`openssl rand -hex 32`). Only the MCP minds this – the
  backup starts anyway, because it never touches the `mcp` section.
- `no principals configured: add "mcp.principals"`: the MCP needs at least one
  identity mapping (name/email/alias → `discord_id`).
- `unknown field "..."`: a typo – unknown keys are rejected on purpose, so a
  misspelled setting fails loudly instead of being ignored.
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
