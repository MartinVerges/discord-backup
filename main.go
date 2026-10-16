// Command discord-backup archives a Discord guild to local JSONL files.
//
// One run = one incremental pass, designed to be invoked from cron every
// 15-30 minutes. Overlapping runs exit harmlessly via a lock file.
//
// Token handling: the bot token is read from the DISCORD_TOKEN environment
// variable or from a file (--token-file / DISCORD_TOKEN_FILE, mode 0600).
// It is never accepted as a CLI argument (visible via ps) and never logged.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"discord-backup/internal/backup"
)

type strList []string

func (s *strList) String() string     { return strings.Join(*s, ",") }
func (s *strList) Set(v string) error { *s = append(*s, v); return nil }

// version is stamped in by GoReleaser (ldflags -X main.version=...).
// Local builds report "dev".
var version = "dev"

func main() {
	os.Exit(run())
}

// printFlags prints all flags with double-dash names (unlike the stdlib's
// PrintDefaults, which renders a single dash).
func printFlags() {
	flag.VisitAll(func(f *flag.Flag) {
		typeName, isBool := flagType(f)
		line := "  --" + f.Name
		if !isBool {
			line += " " + typeName
		}
		fmt.Fprintln(os.Stderr, line)
		usage := f.Usage
		if !isBool && f.DefValue != "" {
			def := f.DefValue
			if typeName == "string" {
				def = fmt.Sprintf("%q", def)
			}
			usage += " (default " + def + ")"
		}
		if isBool && f.DefValue == "true" {
			usage += " (default true)"
		}
		fmt.Fprintln(os.Stderr, "    \t"+usage)
	})
}

// flagType mirrors the stdlib's help display: bools take no value, the rest
// report their type. Concrete flag.Value types are unexported, so probe the
// type name instead.
func flagType(f *flag.Flag) (string, bool) {
	if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
		return "", true
	}
	if f.Name == "channel" {
		return "NAME/ID", false
	}
	t := fmt.Sprintf("%T", f.Value)
	switch {
	case strings.Contains(t, "intValue"):
		return "int", false
	case strings.Contains(t, "stringValue"):
		return "string", false
	case strings.Contains(t, "durationValue"):
		return "duration", false
	case strings.Contains(t, "loatValue"):
		return "float", false
	default:
		return "value", false
	}
}

func run() int {
	var (
		guild        = flag.String("guild", os.Getenv("DISCORD_GUILD_ID"), "Discord guild (server) ID (or DISCORD_GUILD_ID)")
		out          = flag.String("out", envOr("ARCHIVE_PATH", "./discord_archive"), "archive root directory (or ARCHIVE_PATH)")
		tokenFile    = flag.String("token-file", os.Getenv("DISCORD_TOKEN_FILE"), "file containing the bot token (or DISCORD_TOKEN_FILE)")
		includeBots  = flag.Bool("include-bots", false, "also archive bot and webhook messages")
		limit        = flag.Int("limit", 0, "initial backfill cap per channel/thread, newest N (0 = unlimited)")
		sinceStr     = flag.String("since", "", "only archive messages newer than this date (RFC3339 or YYYY-MM-DD), first run only")
		dlAttach     = flag.Bool("download-attachments", false, "download attachments to local disk")
		backfillAtt  = flag.Bool("backfill-attachments", false, "download attachments missing on disk for already archived messages (implies download)")
		editWindow   = flag.Duration("edit-window", 48*time.Hour, "re-verify recent messages for edits/deletes (0 disables)")
		attachMaxMB  = flag.Int("attach-max-mb", 50, "max attachment size in MB")
		attachWorker = flag.Int("attach-workers", 4, "parallel attachment downloads")
		skipEmpty    = flag.Bool("skip-empty", true, "skip channels with no access or zero messages")
		listCh       = flag.Bool("list-channels", false, "list backup-relevant channels and exit")
		quiet        = flag.Bool("quiet", false, "only warnings and errors")
		showVersion  = flag.Bool("version", false, "print version and exit")
	)
	var channels strList
	flag.Var(&channels, "channel", "only archive this channel (name or ID); repeatable")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `discord-backup: incremental Discord guild backup to local JSONL.

Usage:
  discord-backup --guild GUILD_ID [options]

Token (pick one):
  DISCORD_TOKEN env var, or --token-file PATH (file with mode 0600).

Options:
`)
		printFlags()
		fmt.Fprintf(os.Stderr, `
Examples:
  discord-backup --guild 123456789            # incremental pass (cron)
  discord-backup --guild 123456789 --list-channels
  discord-backup --guild 123456789 --channel general --download-attachments

Cron (every 15 minutes):
  */15 * * * * DISCORD_TOKEN_FILE=/etc/discord-backup/token \\
      /usr/local/bin/discord-backup --guild 123456789 --out /var/lib/discord-backup >>/var/log/discord-backup.log 2>&1
`)
	}
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return 0
	}

	level := slog.LevelInfo
	if *quiet {
		level = slog.LevelWarn
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	token, err := loadToken(*tokenFile)
	if err != nil {
		log.Error("token", "err", err)
		return 1
	}
	if *limit < 0 {
		log.Error("--limit must be >= 0")
		return 1
	}
	var since time.Time
	if *sinceStr != "" {
		var err error
		since, err = parseSince(*sinceStr)
		if err != nil {
			log.Error("invalid --since", "err", err)
			return 1
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := backup.Config{
		Token:               token,
		GuildID:             *guild,
		OutDir:              *out,
		Log:                 log,
		IncludeBots:         *includeBots,
		ChannelFilter:       []string(channels),
		Limit:               *limit,
		Since:               since,
		BackfillAttachments: *backfillAtt,
		DownloadAttachments: *dlAttach,
		EditWindow:          *editWindow,
		AttachMaxBytes:      int64(*attachMaxMB) << 20,
		AttachWorkers:       *attachWorker,
		SkipEmpty:           *skipEmpty,
		ListChannels:        *listCh,
	}
	if err := backup.Run(ctx, cfg); err != nil {
		if ctx.Err() != nil {
			log.Error("interrupted")
			return 130
		}
		log.Error("backup failed", "err", err)
		return 1
	}
	return 0
}

// loadToken reads the token from a file (preferred) or the environment.
// The file must not be world-readable.
// parseSince accepts an RFC3339 timestamp or a plain YYYY-MM-DD date
// (midnight UTC) for --since.
func parseSince(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("parse %q: want RFC3339 (2026-01-01T00:00:00Z) or YYYY-MM-DD", s)
}

func loadToken(path string) (string, error) {
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("token file %s must not be group/world accessible (run: chmod 600 %s)", path, path)
		}
		if t := strings.TrimSpace(string(raw)); t != "" {
			return t, nil
		}
		return "", fmt.Errorf("token file %s is empty", path)
	}
	if t := strings.TrimSpace(os.Getenv("DISCORD_TOKEN")); t != "" {
		return t, nil
	}
	return "", fmt.Errorf("no token: set DISCORD_TOKEN or use --token-file")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
