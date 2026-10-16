# Archive format

```
<out>/
  .lock                          # lock file, no need to back it up
  server_<guildID>/
    server_info.json             # name, member count, timestamp
    channels.json                # index: id, name, counters, status
    channel_<id>/
      channel_info.json          # name, topic, category, counters
      messages.jsonl             # all messages, oldest first (append-only)
      edits.jsonl                # new versions of edited messages
      deletes.jsonl              # delete marks {"id","detected_at"}
      pins.json                  # pin IDs + timestamp
      pins_backfill.jsonl        # old pins predating the archive
      attachments/
        <msgID>_<attID>_<file>
      threads/
        thread_<id>/
          thread_info.json
          messages.jsonl / edits.jsonl / deletes.jsonl
          attachments/
```

## Why JSONL?

JSON Lines = one message per line, each line valid JSON by itself:

```json
{"id":"123","channel_id":"456","content":"Hello","timestamp":"2026-01-01T12:00:00.000000+00:00","author":{...},"attachments":[],"embeds":[]}
```

- New messages are **appended** (fast, no matter how large the archive is)
- On interruption, at most the last line is incomplete – detected and removed on the next run
- Readable with standard tools, no special software needed

Each line is the **original Discord message object** – stickers, polls, embeds and reactions are preserved as raw data.

## Behavior details

- `messages.jsonl` is **never modified**: edits appear as full versions in `edits.jsonl`, deletions as marks in `deletes.jsonl`. History stays traceable.
- Pins are an index (`pins.json`); their content is already in `messages.jsonl` – unless it predates the archive, in which case it lives in `pins_backfill.jsonl`.
- `channels.json` / `channel_info.json` / `thread_info.json` are rewritten each run (counters, status, timestamp).

## Querying

```bash
# Last 50 messages of a channel
tail -n 50 channel_987654321098765432/messages.jsonl | jq -r '[.timestamp, .author.username, .content] | @tsv'

# All messages by one author across the server
grep -h '"username":"martin"' server_*/channel_*/messages.jsonl | jq -r .content

# How many edits/deletes were recorded?
wc -l server_*/channel_*/edits.jsonl server_*/channel_*/deletes.jsonl

# List a channel's attachments
ls channel_987654321098765432/attachments/ | head

# Find channels with problems
jq '.[] | select(.status != "ok")' server_*/channels.json
```
