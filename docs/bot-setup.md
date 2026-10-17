# Setting up the Discord bot

One time, about 5 minutes. The bot only needs read access – it never writes to your server.

## 1. Create the application and bot

1. Open https://discord.com/developers/applications/
2. **New Application** → give it a name (e.g. `Backup`) → Create
3. On the left, **Bot** → **Add Bot** → **Yes, do it**
4. Under **Token**, click **Reset Token** → copy the token and store it safely (it is shown in plain text only once)

Put the token into the configuration file (the one place the tool reads it from):

```bash
cp config.example.json config.json    # then set "guild_id", "archive_dir", "token"
chmod 600 config.json
```

> The tool refuses a config that holds tokens but is readable by group/others. The
> token is never accepted as a command-line argument (visible to everyone via `ps`)
> and never logged.

## 2. Enable intents

Under **Bot → Privileged Gateway Intents**:

- ✅ **Message Content Intent** – required, otherwise all message contents are empty
- ✅ **Server Members Intent** – for the exact member count (works without it too, then approximate)

Click **Save Changes** afterwards.

## 3. Invite the bot to your server

1. On the left, **OAuth2 → URL Generator**
2. Under **Scopes**, check only `bot`
3. Under **Bot Permissions**, check:
   - ✅ View Channels
   - ✅ Read Message History
4. Copy the generated URL, open it in your browser, select your server, authorize

## 4. Find the server ID

In Discord: Settings (gear icon) → **Advanced** → enable **Developer Mode**.
Then right-click the server name → **Copy Server ID**.

## 5. Verify access

```bash
cp config.example.json config.json     # guild_id + token
chmod 600 config.json
discord-backup --config config.json --list-channels
```

This prints a table of all archivable channels. Then a test run limited to a
single channel (`"channels": ["general"]` under `backup`):

```bash
discord-backup --config config.json
```

If the run ends with `status=ok`, everything is set. On `forbidden`/`no_access`, see [troubleshooting.md](troubleshooting.md).

## 6. Grant access to private channels (optional)

The bot only sees what its roles may see. Per channel or category:

Channel → **Edit Channel → Permissions** → add the bot role → ✅ **View Channel**, ✅ **Read Message History**.

Tip: set permissions on the **category** – they inherit to all channels below it.

Next: [usage.md](usage.md) (usage) and [automation.md](automation.md) (automation).
