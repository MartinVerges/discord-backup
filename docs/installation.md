# Installation

Requirement: Go 1.24 or newer (`go version`).

```bash
git clone <repo-url> discord-backup
cd discord-backup
go build -o discord-backup .
```

That's it – the binary has no runtime dependencies. Tests (optional):

```bash
go test ./...
```

## Cross-platform builds

The binary runs anywhere, it just needs to be built for the target system:

```bash
GOOS=linux   GOARCH=amd64 go build -o discord-backup-linux .
GOOS=windows GOARCH=amd64 go build -o discord-backup.exe .
GOOS=darwin  GOARCH=arm64 go build -o discord-backup-mac .
```

## Installing

Put the binary somewhere on your `PATH`, e.g.:

```bash
sudo install -m 755 discord-backup /usr/local/bin/discord-backup
```

Next: [bot-setup.md](bot-setup.md) (setting up the Discord bot).
