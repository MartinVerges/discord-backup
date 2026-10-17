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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"discord-backup/internal/backup"
	"discord-backup/internal/mcp"
)

type strList []string

func (s *strList) String() string     { return strings.Join(*s, ",") }
func (s *strList) Set(v string) error { *s = append(*s, v); return nil }

// version is stamped in by GoReleaser (ldflags -X main.version=...).
// Local builds report "dev".
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "mcp":
			os.Exit(runMCP(os.Args[2:]))
		case "healthcheck":
			os.Exit(runHealthcheck(os.Args[2:]))
		}
	}
	os.Exit(run())
}

// runHealthcheck probes a running MCP HTTP endpoint and exits 0 on success.
// It exists so a container healthcheck works on distroless, which ships no
// shell, curl or wget.
func runHealthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	url := fs.String("url", "http://127.0.0.1:8765/healthz", "health endpoint to probe")
	timeout := fs.Duration("timeout", 3*time.Second, "request timeout")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `discord-backup healthcheck: exit 0 when the MCP HTTP endpoint answers.

Usage:
  discord-backup healthcheck --url http://127.0.0.1:8765/healthz

Options:
  --url URL        health endpoint to probe (default http://127.0.0.1:8765/healthz)
  --timeout dur    request timeout (default 3s)
`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	resp, err := (&http.Client{Timeout: *timeout}).Get(*url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s\n", resp.Status)
		return 1
	}
	return 0
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
		every        = flag.Duration("every", 0, "keep running: wait this long after each pass, then back up again (0 = run once)")
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
  discord-backup mcp --config mcp.json [--principal NAME | --http [--addr ADDR]]
  discord-backup healthcheck --url http://127.0.0.1:8765/healthz

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
  discord-backup --guild 123456789 --every 15m   # self-scheduling service
  discord-backup mcp --config mcp.json        # read-only MCP server (stdio)

Cron (every 15 minutes):
  */15 * * * * DISCORD_TOKEN_FILE=/etc/discord-backup/token \\
      /usr/local/bin/discord-backup --guild 123456789 --out /var/lib/discord-backup >>/var/log/discord-backup.log 2>&1

Service (no cron, e.g. in Docker): use --every, which waits after each pass:
  discord-backup --guild 123456789 --out /var/lib/discord-backup --every 15m

See docs/mcp.md for permission-scoped, read-only access over MCP.
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
	if *every > 0 && *listCh {
		log.Error("--every cannot be combined with --list-channels")
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
	if *every <= 0 {
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

	// Service mode: run a pass, then wait --every after it finished (not from
	// its start), so passes never overlap. A failing pass is logged and retried
	// on the next tick instead of ending the loop, so a transient Discord
	// outage does not stop the schedule.
	log.Info("running as a service", "every", every.String())
	for {
		if err := backup.Run(ctx, cfg); err != nil {
			if ctx.Err() != nil {
				log.Info("stopping")
				return 0
			}
			log.Error("backup failed", "err", err, "retry_in", every.String())
		}
		t := time.NewTimer(*every)
		select {
		case <-ctx.Done():
			t.Stop()
			log.Info("stopping")
			return 0
		case <-t.C:
		}
	}
}

// runMCP runs the read-only, permission-scoped MCP server.
func runMCP(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", os.Getenv("DISCORD_MCP_CONFIG"), "path to the MCP config JSON (or DISCORD_MCP_CONFIG)")
	principal := fs.String("principal", "", "principal (name/email/discord_id) for the stdio transport")
	httpMode := fs.Bool("http", false, "serve the HTTP transport instead of stdio")
	addr := fs.String("addr", "", "HTTP listen address (default: config http_addr, else 127.0.0.1:8765)")
	quiet := fs.Bool("quiet", false, "only warnings and errors")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `discord-backup mcp: read-only MCP access to an archive, scoped by
the caller's Discord channel permissions.

Usage:
  discord-backup mcp --config mcp.json [--principal NAME]
  discord-backup mcp --config mcp.json --http [--addr 127.0.0.1:8765]

Options:
`)
		fs.VisitAll(func(f *flag.Flag) {
			line := "  --" + f.Name
			if _, isBool := f.Value.(interface{ IsBoolFlag() bool }); !isBool {
				line += " string"
			}
			fmt.Fprintln(os.Stderr, line)
			fmt.Fprintln(os.Stderr, "    \t"+f.Usage)
		})
		fmt.Fprintf(os.Stderr, `
The archive (server_<id>/) must exist; permissions.json inside it is the
offline fallback when Discord is unreachable. See docs/mcp.md.
`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Println(version)
		return 0
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "error: --config is required (see docs/mcp.md)")
		return 2
	}
	level := slog.LevelInfo
	if *quiet {
		level = slog.LevelWarn
	}
	// Diagnostics go to stderr so stdout stays a clean JSON-RPC channel.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := mcp.LoadConfig(*configPath)
	if err != nil {
		log.Error("config", "err", err)
		return 1
	}
	settings, err := cfg.Settings()
	if err != nil {
		log.Error("config", "err", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv, err := mcp.New(ctx, settings, log)
	if err != nil {
		log.Error("open archive", "err", err)
		return 1
	}

	if *httpMode {
		listen := *addr
		if listen == "" {
			listen = settings.HTTPAddr
		}
		if listen == "" {
			listen = "127.0.0.1:8765"
		}
		withToken := 0
		for _, p := range settings.Principals {
			if p.Token != "" {
				withToken++
			}
		}
		if withToken == 0 {
			log.Error("http transport needs at least one principal with a \"token\"")
			return 1
		}
		if err := srv.ServeHTTP(ctx, listen, log); err != nil {
			log.Error("http server", "err", err)
			return 1
		}
		return 0
	}

	pr, err := settings.StdioPrincipal(*principal)
	if err != nil {
		log.Error("principal", "err", err)
		return 1
	}
	if settings.Token == "" {
		log.Info("running in snapshot-only mode (no token configured); set \"token\" for live permission refresh")
	}
	log.Info("mcp stdio ready", "principal", pr.Name, "discord_id", pr.DiscordID, "archive", settings.ArchiveDir)
	if err := srv.ServeStdio(ctx, pr); err != nil {
		log.Error("stdio", "err", err)
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
		fi, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("token file %s must not be group/world accessible (run: chmod 600 %s)", path, path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
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
