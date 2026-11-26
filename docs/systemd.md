# systemd-Service für discord-backup

Der Discord-Backup läuft als nativer Go-Binary direkt auf dem Host (kein Docker mehr),
verwaltet durch systemd.

## Installation

1. Binary kompilieren und installieren:
   ```bash
   CGO_ENABLED=0 go build -o /usr/local/bin/discord-backup .
   ```

2. `config.json` anpassen: `archive_dir` auf das lokale Archiv-Verzeichnis setzen.

3. Unit-Datei installieren:
   ```bash
   sudo cp discord-backup.service /etc/systemd/system/discord-backup.service
   sudo systemctl daemon-reload
   sudo systemctl enable --now discord-backup.service
   ```

## Befehle

```bash
sudo systemctl status discord-backup.service      # Status
sudo systemctl restart discord-backup.service     # Neustart
sudo systemctl stop discord-backup.service        # Stoppen
journalctl -u discord-backup.service -f           # Logs folgen
```

## Mount-Check

Die Unit prüft mit `ExecStartPre`, ob das ZFS-Archiv-Verzeichnis existiert und
beschreibbar ist, bevor der Backup startet. Fehlt der Mount, startet der Dienst nicht
(verhindert, dass ins Leere gesichert wird).
