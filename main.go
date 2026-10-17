// Command discord-backup archives a Discord guild to local JSONL files and
// serves it read-only over MCP.
//
// Everything is configured in one file (--config, default "config.json"): the
// bot token, the guild, the archive directory, the backup schedule and the MCP
// settings. Both "discord-backup" and "discord-backup mcp" read that same
// file. There is no token file, no separate MCP config and no environment
// fallback, so a secret has exactly one home and one rule: a config holding
// tokens must be mode 0600. The token is never passed as a CLI argument and
// never logged.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"discord-backup/internal/backup"
	"discord-backup/internal/config"
	"discord-backup/internal/mcp"
)

// defaultConfigPath is used when --config is not given.
const defaultConfigPath = "config.json"

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
		case "principals":
			os.Exit(runPrincipals(os.Args[2:]))
		}
	}
	os.Exit(runBackup(os.Args[1:]))
}

// runHealthcheck probes a running MCP HTTP endpoint and exits 0 on success.
// It exists so a container healthcheck works on distroless, which ships no
// shell, curl or wget. The endpoint can be derived from the configuration
// (mcp.http_addr) so no address is duplicated in the compose file.
func runHealthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "configuration file; the endpoint comes from mcp.http_addr")
	url := fs.String("url", "", "health endpoint to probe (overrides --config)")
	principal := fs.String("principal", "", "also make an authenticated MCP call as this principal (\"auto\" = any with a token)")
	timeout := fs.Duration("timeout", 3*time.Second, "request timeout")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `discord-backup healthcheck: exit 0 when the MCP HTTP endpoint answers.

Usage:
  discord-backup healthcheck --config /etc/discord-backup.json
  discord-backup healthcheck --config /etc/discord-backup.json --principal auto
  discord-backup healthcheck --url http://127.0.0.1:8765/healthz

Options:
  --config PATH    configuration file; probes http://<mcp.http_addr>/healthz
                   (also fails when the config itself is invalid)
  --url URL        health endpoint to probe (default http://127.0.0.1:8765/healthz)
  --principal P    additionally do a real MCP call (initialize, tools/list,
                   whoami) with P's bearer token: proves auth, archive and
                   permissions. One round trip, nothing is changed.
  --timeout dur    request timeout (default 3s)
`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	var base string
	target := *url
	if target == "" {
		addr := "127.0.0.1:8765"
		if *configPath != "" {
			opts, err := config.Load(*configPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
				return 1
			}
			if opts.MCP.HTTPAddr != "" {
				addr = opts.MCP.HTTPAddr
			}
		}
		base = "http://" + probeAddr(addr)
		target = base + "/healthz"
	} else if u, err := neturl.Parse(target); err == nil && u.Host != "" {
		base = u.Scheme + "://" + u.Host
	} else {
		base = strings.TrimSuffix(target, "/healthz")
	}

	client := &http.Client{Timeout: *timeout}
	resp, err := client.Get(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s\n", resp.Status)
		return 1
	}
	if *principal == "" {
		return 0
	}

	// The authenticated round trip: the part that says whether the server can
	// actually serve this identity, not just whether the port is open.
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "healthcheck: --principal needs --config to look up the token")
		return 2
	}
	opts, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	pr, err := pickPrincipal(opts.MCP.Principals, *principal)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	if strings.TrimSpace(pr.Token) == "" {
		fmt.Fprintf(os.Stderr, "healthcheck: principal %q has no \"token\" in the config; the http transport needs one\n", pr.Name)
		return 1
	}
	result, err := mcpSmoke(client, base, pr.Token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	printMCPCheck(os.Stdout, base, result)
	return 0
}

// probeAddr turns a listen address into something dialable: a wildcard bind
// (":8765", "0.0.0.0:8765", "[::]:8765") is probed on loopback.
func probeAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// runBackup runs one backup pass, or – when the config sets backup.every – a
// self-scheduling loop, which is what lets a container be a backup service
// without cron.
func runBackup(args []string) int {
	fs := flag.NewFlagSet("discord-backup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", defaultConfigPath, "configuration file (env-free, holds the token)")
	listCh := fs.Bool("list-channels", false, "list backup-relevant channels and exit")
	quiet := fs.Bool("quiet", false, "only warnings and errors")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `discord-backup: incremental Discord guild backup to local JSONL.

Usage:
  discord-backup [--config %s] [--list-channels]
  discord-backup mcp --config %s [--principal NAME | --http]
  discord-backup healthcheck --config /etc/discord-backup.json
  discord-backup principals --config %s [--write]

Everything lives in one configuration file (default %q): token, guild,
archive_dir, backup settings and MCP settings. Start from config.example.json.

Options:
`, defaultConfigPath, defaultConfigPath, defaultConfigPath, defaultConfigPath)
		printFlags(fs)
		fmt.Fprintf(os.Stderr, `
Examples:
  discord-backup                              # one pass, ./config.json
  discord-backup --config /etc/db.json --list-channels
  discord-backup mcp --config config.json     # read-only MCP server (stdio)
  discord-backup principals --config config.json  # who can reach it (mcp.principals)

Service without cron – configured, not flagged:
  {"backup": {"every": "15m", "download_attachments": true}}
waits 15m after each pass, then backs up again.
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
	level := slog.LevelInfo
	if *quiet {
		level = slog.LevelWarn
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	opts, err := config.Load(*configPath)
	if err != nil {
		log.Error("config", "err", err)
		return 1
	}
	if opts.Token == "" {
		log.Error("no bot token: set \"token\" in " + *configPath)
		return 1
	}
	if err := opts.CheckBackup(); err != nil {
		log.Error("config", "err", err)
		return 1
	}
	if *listCh && opts.Backup.Every > 0 {
		log.Error("--list-channels cannot be combined with backup.every")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := backup.Config{
		Token:               opts.Token,
		GuildID:             opts.GuildID,
		OutDir:              opts.ArchiveDir,
		Log:                 log,
		IncludeBots:         opts.Backup.IncludeBots,
		ChannelFilter:       opts.Backup.Channels,
		Limit:               opts.Backup.Limit,
		Since:               opts.Backup.Since,
		BackfillAttachments: opts.Backup.BackfillAttachments,
		DownloadAttachments: opts.Backup.DownloadAttachments,
		EditWindow:          opts.Backup.EditWindow,
		AttachMaxBytes:      int64(opts.Backup.AttachMaxMB) << 20,
		AttachWorkers:       opts.Backup.AttachWorkers,
		SkipEmpty:           opts.Backup.SkipEmpty,
		ListChannels:        *listCh,
	}

	every := opts.Backup.Every
	if every <= 0 {
		if err := backup.Run(ctx, cfg); err != nil {
			if errors.Is(err, backup.ErrAnotherRunActive) {
				// Not a failure: the previous run is still working.
				log.Info("another backup run is active, exiting")
				return 0
			}
			if ctx.Err() != nil {
				log.Error("interrupted")
				return 130
			}
			log.Error("backup failed", "err", err)
			return 1
		}
		return 0
	}

	// Service mode: run a pass, then wait backup.every after it finished (not
	// from its start), so passes never overlap. A failing pass is logged and
	// retried on the next tick instead of ending the loop, so a transient
	// Discord outage cannot stop the schedule.
	log.Info("running as a service", "every", every.String(), "archive", opts.ArchiveDir)
	for {
		wait := every
		switch err := backup.Run(ctx, cfg); {
		case err == nil:
		case errors.Is(err, backup.ErrAnotherRunActive):
			// Someone else holds the archive. Do not sit idle for the whole
			// interval: they are working right now, so check back soon.
			wait = lockRetry(every)
			log.Info(err.Error(), "retry_in", wait.String())
		default:
			if ctx.Err() != nil {
				log.Info("stopping")
				return 0
			}
			log.Error("backup failed", "err", err, "retry_in", every.String())
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			log.Info("stopping")
			return 0
		case <-t.C:
		}
	}
}

// lockRetryCap bounds how long a busy archive is left alone. An overlapping run
// usually finishes in seconds, so waiting the whole interval (which may be
// hours) would silently stall the schedule.
const lockRetryCap = time.Minute

// lockRetry is how long to wait before re-checking a busy lock.
func lockRetry(every time.Duration) time.Duration {
	if every <= 0 || every > lockRetryCap {
		return lockRetryCap
	}
	return every
}

// inContainer reports whether we are very likely inside a container, where a
// loopback bind cannot be reached through a published port.
func inContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil { // podman
		return true
	}
	raw, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(raw)
	return strings.Contains(s, "docker") || strings.Contains(s, "kubepods") || strings.Contains(s, "containerd")
}

// isLoopbackAddr reports whether a listen address only accepts connections from
// inside its own network namespace (so a published port cannot reach it).
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" { // wildcard: reachable
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// runMCP runs the read-only, permission-scoped MCP server off the same config
// file the backup uses.
func runMCP(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", defaultConfigPath, "configuration file (the same one the backup reads)")
	principal := fs.String("principal", "", "principal (name/email/discord_id) for the stdio transport")
	httpMode := fs.Bool("http", false, "serve the HTTP transport instead of stdio")
	quiet := fs.Bool("quiet", false, "only warnings and errors")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `discord-backup mcp: read-only MCP access to an archive, scoped by
the caller's Discord channel permissions.

Usage:
  discord-backup mcp --config %s [--principal NAME]
  discord-backup mcp --config %s --http

Options:
`, defaultConfigPath, defaultConfigPath)
		printFlags(fs)
		fmt.Fprintf(os.Stderr, `
The archive (archive_dir/server_<id>/) must exist; permissions.json inside it
is the offline fallback when Discord is unreachable. The listen address for
--http is mcp.http_addr (default 127.0.0.1:8765). See docs/mcp.md.
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
	level := slog.LevelInfo
	if *quiet {
		level = slog.LevelWarn
	}
	// Diagnostics go to stderr so stdout stays a clean JSON-RPC channel.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	opts, err := config.Load(*configPath)
	if err != nil {
		log.Error("config", "err", err)
		return 1
	}
	if err := opts.CheckMCP(); err != nil {
		log.Error("config", "err", err)
		return 1
	}
	settings := mcp.Settings{
		ArchiveDir:          opts.ArchiveDir,
		GuildID:             opts.GuildID,
		Token:               opts.Token,
		ShowDeleted:         opts.MCP.ShowDeleted,
		RefreshTTL:          opts.MCP.RefreshTTL,
		ChannelTTL:          opts.MCP.ChannelTTL,
		MemberTTL:           opts.MCP.MemberTTL,
		RetryTTL:            opts.MCP.RetryTTL,
		Timeout:             opts.MCP.Timeout,
		CheckPrivateThreads: opts.MCP.CheckPrivateThreads,
		AuditLog:            opts.MCP.AuditLog,
		DefaultPrincipal:    opts.MCP.DefaultPrincipal,
		HTTPAddr:            opts.MCP.HTTPAddr,
		Principals:          opts.MCP.Principals,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv, err := mcp.New(ctx, settings, log)
	if err != nil {
		log.Error("open archive", "err", err)
		return 1
	}

	if *httpMode {
		listen := settings.HTTPAddr
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
			log.Error(`the http transport needs at least one principal with a "token"`)
			return 1
		}
		if isLoopbackAddr(listen) && inContainer() {
			log.Warn("listening on loopback inside a container: a published port on the host cannot reach it",
				"addr", listen,
				"fix", `set "mcp.http_addr" to "0.0.0.0:8765" and publish it on the host as "127.0.0.1:8765:8765"`)
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

// printFlags prints all flags with double-dash names (unlike the stdlib's
// PrintDefaults, which renders a single dash).
func printFlags(fs *flag.FlagSet) {
	fs.VisitAll(func(f *flag.Flag) {
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
