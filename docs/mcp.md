# MCP access (read-only, permission-scoped)

`discord-backup mcp` serves an archive over the
[Model Context Protocol](https://modelcontextprotocol.io) so an LLM (or any
MCP client) can search and read it. **It only ever returns content the caller
is allowed to read in Discord.** It never writes to the archive, never
uploads anything, and never fetches anything but the Discord API.

```
MCP client ──▶ discord-backup mcp ──▶ archive (JSONL, read-only)
                      │
                      └──▶ Discord API (permissions only, optional)
```

## How access is decided

Discord exposes no "what may user X read" endpoint, so the server reproduces
Discord's permission algorithm for every request:

```
base = @everyone.permissions | OR(role.permissions for each of the user's roles)
if base has ADMINISTRATOR → everything
else apply overwrites in tiers: @everyone → roles → member
     (a category's overwrites apply to its channels first)
```

A channel counts as **readable** when the result has **both** `VIEW_CHANNEL`
and `READ_MESSAGE_HISTORY`. This is deliberately strict: a role that may see a
channel but not its history sees nothing in the archive, because the archive
*is* history.

Threads inherit their parent channel. **Private threads** are only readable if
the caller is a thread member; since the archive cannot prove that on its own
they are hidden by default (see `check_private_threads` below).

### Live vs. offline

Every backup run writes `server_<id>/permissions.json` – a snapshot of roles,
channel overwrites and member roles. The MCP server:

1. reads **live** channel overwrites, roles and the member's roles (cached:
   channels/roles 5 min, member roles 60 s), and
2. falls back to the snapshot in `permissions.json` whenever Discord is
   unreachable or the token is missing.

So access keeps working during a Discord outage, and the archive itself
carries the information needed to enforce it.

After a failed live lookup the server pauses live lookups for `retry_ttl`
(default 5 min) and answers from cache/snapshot instead, so an outage cannot
make every request wait out its own timeout. This also bounds how long
concurrent requests can queue behind a permission lookup: the resolver lock is
only ever held for one (rare) live attempt, never indefinitely. Discord is
retried automatically once the pause elapses.

**Fail-closed:** if no source can prove access – unknown user, missing roles,
no snapshot and no Discord – the caller simply sees nothing.

`@everyone` applies only to proven guild members. A user Discord reports as
"not a member" (or who is neither resolvable live nor present in the snapshot)
is denied everything, even when `@everyone` grants read access. This matters
for a guild where `@everyone` can read: without this rule a stale or foreign
principal could read the whole archive.

## Requirements

- An existing archive (`server_<id>`) produced by `discord-backup`.
- The **same configuration file** the backup uses (see
  [usage.md](usage.md)); the MCP settings are its `mcp` section.
- For **live** permission checks: the bot token in that file (`token`). The bot
  must be able to read the guild's channels, roles and members.
- Without a token the server runs **snapshot-only**: roles and member roles
  come from the last backup run.

## Configuration

There is no separate MCP config: `discord-backup mcp` reads the *same* file as
the backup and uses its `mcp` section. Because the file holds tokens (the bot
token and the principals' bearer tokens) it **must be mode `0600`**; the server
refuses to start otherwise.

```json
{
  "guild_id": "123456789012345678",
  "archive_dir": "/var/lib/discord-backup",
  "token": "BOT_TOKEN_SAME_AS_THE_BACKUP",

  "backup": { "download_attachments": true },

  "mcp": {
    "http_addr": "127.0.0.1:8765",
    "default_principal": "martin",
    "deleted": "hide",
    "refresh_ttl": "60s",
    "channel_ttl": "5m",
    "member_ttl": "60s",
    "retry_ttl": "5m",
    "timeout": "5s",
    "check_private_threads": false,
    "audit_log": "/var/log/discord-backup/mcp-audit.jsonl",
    "principals": [
      {
        "name": "martin",
        "discord_id": "111111111111111111",
        "email": "martin@example.com",
        "aliases": ["martinverges"],
        "token": "a-long-random-string-for-the-http-transport"
      },
      {
        "name": "hildrun",
        "discord_id": "222222222222222222",
        "aliases": ["hildrunverges"]
      }
    ]
  }
}
```

```bash
chmod 600 config.json        # required: the file holds tokens
# generate each principal token with: openssl rand -hex 32
```

| Key under `mcp` | Default | Meaning |
|---|---|---|
| `http_addr` | `127.0.0.1:8765` | Listen address for `--http` |
| `default_principal` | – | Principal used by stdio when `--principal` is omitted |
| `deleted` | `hide` | `hide` deleted messages, or `show` them (marked `deleted`) |
| `refresh_ttl` | `60s` | How often the archive is rescanned for new data |
| `channel_ttl` | `5m` | Cache lifetime for channel overwrites + roles |
| `member_ttl` | `60s` | Cache lifetime for one member's roles |
| `retry_ttl` | `5m` | Pause live lookups this long after a failure, then retry |
| `timeout` | `5s` | Bound per live lookup, so an outage degrades quickly |
| `check_private_threads` | `false` | Verify private-thread membership live (extra API calls) |
| `audit_log` | – | Optional JSONL file, one line per tool call |
| `principals` | – | At least one; see below |

The keys outside `mcp` (`guild_id`, `archive_dir`, `token`) are shared with the
backup – `archive_dir` is where `server_<id>` lives.

The MCP only enforces the `mcp` section (principals, `deleted`, the HTTP token
rule); settings under `backup` are ignored here, just as the backup ignores
`mcp`. A malformed value (a bad duration, a negative number) fails in both.

### Principals

A **principal** maps a person – addressed by `name`, `email`, `discord_id` or
any `aliases` entry – to the Discord user ID whose permissions gate everything
they can see. That is the whole identity mapping: no passwords, no accounts.

`token` is the bearer token the HTTP transport requires for that principal.
Tokens must be unique; if `http_addr` is set, at least one principal needs one
(the server also refuses to start the HTTP transport without any token).

## Running

### stdio (Claude Desktop, Claude Code, …)

```bash
discord-backup mcp --config config.json --principal martin
```

The JSON-RPC protocol runs on stdin/stdout; all logs go to stderr.

### HTTP

```bash
discord-backup mcp --config config.json --http
```

- `POST /mcp` – JSON-RPC 2.0, answered as `application/json`, or as
  `text/event-stream` when the client sends `Accept: text/event-stream`.
  Batch arrays are supported.
- `GET /healthz` – `{"status":"ok"}`.
- Auth: `Authorization: Bearer <principal token>` (or `X-MCP-Token`). Requests
  without a valid token get `401`.

The listen address is `mcp.http_addr` (default `127.0.0.1:8765`). Bind to
`127.0.0.1` unless you put a TLS reverse proxy in front – **except in a
container**, where loopback is unreachable through a published port: use
`0.0.0.0:8765` there and publish the port on the host as `127.0.0.1:8765:8765`
(see [docker.md](docker.md#host-vs-container-address)).

#### Healthcheck

`discord-backup healthcheck --config /etc/discord-backup.json` probes
`http://<mcp.http_addr>/healthz` and exits `0` exactly when it answers `200`
(a wildcard address like `0.0.0.0:8765` is probed on loopback). Use it where
there is no shell or `curl` (notably the distroless image); `--url` overrides
the derived endpoint. See [docker.md](docker.md).

Add `--principal` and it stops being a ping and becomes a real MCP check:

```bash
discord-backup healthcheck --config /etc/discord-backup.json --principal auto
```

It sends `initialize`, `tools/list` and a `whoami` call with that principal's
bearer token and prints what came back (server version, tool list, identity,
how many channels/threads/messages that identity may see). Everything read-only,
one round trip - the fast way to tell "port open" from "actually serving me".
`auto` picks any principal with a token; a name/email/alias/`discord_id` picks
one. Without `curl` on the host, this is also the easiest probe from inside a
container:

```bash
docker compose exec mcp /usr/local/bin/discord-backup \
  healthcheck --config /etc/discord-backup.json --principal auto
```

### Flags

| Flag | Meaning |
|---|---|
| `--config PATH` | Configuration file, the same one the backup reads (default `config.json`) |
| `--principal NAME` | Principal for stdio (`name`, `email`, `discord_id` or alias) |
| `--http` | Serve HTTP instead of stdio |
| `--quiet` | Only warnings and errors |
| `--version` | Print version and exit |

## Client setup (Claude Desktop)

```json
{
  "mcpServers": {
    "discord-archive": {
      "command": "/usr/local/bin/discord-backup",
      "args": ["mcp", "--config", "/etc/discord-backup.json", "--principal", "martin"]
    }
  }
}
```

## Tools

All tools are read-only and silently bounded by the caller's permissions.

| Tool | Arguments | Returns |
|---|---|---|
| `whoami` | – | Principal, guild, permission source, visible counts |
| `list_channels` | `include_threads?`, `include_empty?` | Readable channels (name, id, category, message/thread counts) |
| `list_threads` | `channel` | Readable threads of one channel |
| `search_messages` | `query?`, `channel?`, `author?`, `after?`, `before?`, `limit?`, `order?` | Matching messages (channel, author, time, snippet, ID) |
| `get_message` | `message_id` | One message including its full raw JSON payload |
| `get_context` | `message_id`, `before?`, `after?` | Surrounding messages in the same channel/thread |
| `list_authors` | `query?`, `limit?` | Most active authors among the visible messages |

Notes:

- `search_messages` matches **all** query terms as case-insensitive
  substrings (AND), and terms also match attachment filenames. An empty
  `query` returns the newest visible messages.
- `after`/`before` accept RFC 3339 or `YYYY-MM-DD`.
- `limit` defaults to 50 (max 500); when more matches exist the result says so.
- Deleted messages are hidden by default (`deleted: "hide"`).

### Example

```
> whoami
{ "principal": "hildrun", "permission_source": "live",
  "visible_channels": 41, "visible_threads": 187, "visible_messages": 38214 }

> search_messages { "query": "ceph upgrade", "author": "martin", "limit": 5 }
{ "total": 12, "count": 5,
  "results": [ { "message_id": "…", "channel": "ceph-rust",
                 "author": "martinverges", "timestamp": "2026-03-04T09:12:44Z",
                 "snippet": "…the ceph upgrade is planned for Tuesday…" } ] }

> get_context { "message_id": "…", "before": 2, "after": 2 }
```

## Audit log

With `audit_log` set, every tool call appends one JSON line:

```json
{"ts":"2026-03-05T10:00:00Z","principal":"hildrun","discord_id":"…","tool":"search_messages","args":{"query":"ceph"},"ok":true,"count":5}
```

## Performance

Measured on a synthetic archive of **132,000 messages / 265 logs**:

| Operation | Time |
|---|---|
| Load (start-up index) | ~200 ms |
| Search (any query) | ~8 ms (miss ~2 ms) |
| Refresh with no changes | ~6 ms |

Memory: ~66 MB heap for 132k messages; raw payloads stay on disk and are read
by ID on demand. Queries after the first are served entirely from memory, so
the first `whoami` may include one live permission refresh (~1 s), and every
later call is sub-second.

## Security notes

- Read-only: no tool writes to the archive; the server downloads nothing.
- Every token – the bot token and each principal's bearer token – lives in the
  one configuration file, which must be mode `0600`; the server refuses a config
  that holds tokens but is group/world-readable. There is no token file and no
  environment variable, and tokens are never logged or accepted as CLI
  arguments.
- HTTP requires a per-principal bearer token (constant-time comparison).
- Content outside the caller's permissions is filtered before it leaves the
  process — including search hits, message lookups, context and channel lists.

## Limitations

- **Channels deleted in Discord** drop out of the permission data, so their
  archived messages become unreachable through the MCP (the files stay on
  disk). If an archive must stay queryable, archive/lock a channel instead of
  deleting it, or accept that only administrators can reach it while it exists.
- **Removed members** are denied as soon as Discord reports them as non-members
  (or the snapshot no longer lists them); `@everyone` never applies to them.
- **Private threads** are hidden unless `check_private_threads` is enabled and
  the bot token is available (then membership is verified live, fail-closed).
- **Large guilds** (> 10,000 members) cannot be fully captured in the snapshot;
  offline access for them needs the live path.
- Threads archived by an **older version** have no recorded type and are
  therefore hidden until the next backup run records them (`whoami` reports
  `threads_without_type`).
- Search is case-insensitive substring matching, not fuzzy or ranked.
