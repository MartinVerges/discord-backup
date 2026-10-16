# Automation

One run syncs everything since the last one. Just invoke it repeatedly – a lock file
(`$OUT/.lock`) keeps two runs from overlapping; a skipped or interrupted run is picked up
by the next one.

## cron (Linux/macOS)

`crontab -e`, every 15 minutes:

```
*/15 * * * * /usr/local/bin/discord-backup --guild YOUR_SERVER_ID --out /var/lib/discord-backup --download-attachments >>/var/log/discord-backup.log 2>&1
```

With a token file instead of env (recommended if multiple users share the system):

```
*/15 * * * * /usr/local/bin/discord-backup --guild YOUR_SERVER_ID --out /var/lib/discord-backup --download-attachments --token-file /etc/discord-backup/token >>/var/log/discord-backup.log 2>&1
```

## systemd timer (Linux)

`/etc/systemd/system/discord-backup.service`:

```ini
[Unit]
Description=Discord guild backup
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/discord-backup --guild YOUR_SERVER_ID --out /var/lib/discord-backup --download-attachments --token-file /etc/discord-backup/token
```

`/etc/systemd/system/discord-backup.timer`:

```ini
[Unit]
Description=Discord guild backup every 15 minutes

[Timer]
OnBootSec=5min
OnUnitActiveSec=15min

[Install]
WantedBy=timers.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now discord-backup.timer
```

## launchd (macOS)

`~/Library/LaunchAgents/com.example.discord-backup.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.example.discord-backup</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/discord-backup</string>
    <string>--guild</string><string>YOUR_SERVER_ID</string>
    <string>--out</string><string>/Users/YOU/discord-backup</string>
    <string>--download-attachments</string>
    <string>--token-file</string><string>/Users/YOU/.discord-backup-token</string>
  </array>
  <key>StartInterval</key><integer>900</integer>
  <key>StandardOutPath</key><string>/tmp/discord-backup.log</string>
  <key>StandardErrorPath</key><string>/tmp/discord-backup.log</string>
</dict>
</plist>
```

```bash
launchctl load ~/Library/LaunchAgents/com.example.discord-backup.plist
```

## Backing up the archive itself

The result is plain files – back them up with your usual file backup (restic, rsync, Time Machine, …).
`.lock` does not need to be backed up.
