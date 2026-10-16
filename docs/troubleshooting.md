# Troubleshooting

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

- `no token`: `DISCORD_TOKEN` or `--token-file` is missing.
- `token file … must not be group/world accessible`: `chmod 600 <file>`.
- `401 Unauthorized`: token wrong/expired → regenerate via **Reset Token** in the portal.

## Rate limits / slow first run

Normal: Discord throttles requests, the tool complies and patiently backfills history.
Interrupting is safe – the next run continues. For very active servers, skip
`--download-attachments` on the first run and catch up later.

## `attachments_failed > 0`

Individual downloads (expired CDN links, files over `--attach-max-mb`) may fail while the run
continues. Missing files are retried on the next run as long as the URL is still valid.
