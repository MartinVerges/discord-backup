// Package config holds the one configuration file the whole tool reads.
//
// Both the backup and the MCP server take the same file ("discord-backup
// --config config.json" and "discord-backup mcp --config config.json"): the
// bot token, the guild, the archive directory, the backup schedule and the MCP
// settings all live in it. There is deliberately no token file, no separate
// mcp.json and no environment fallback, so secrets have exactly one home.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
)

// Config is the on-disk JSON schema. Unknown fields are rejected, so a typo is
// an error instead of a silently ignored setting.
type Config struct {
	// GuildID is the Discord server to archive.
	GuildID string `json:"guild_id"`
	// ArchiveDir is the archive root (the backup "out" directory, containing
	// server_<id>/).
	ArchiveDir string `json:"archive_dir"`
	// Token is the bot token, used by both the backup and the MCP server's
	// live permission lookups. Without it the MCP runs on the archive's
	// permissions snapshot only.
	Token string `json:"token"`

	Backup Backup `json:"backup"`
	MCP    MCP    `json:"mcp"`
}

// Backup controls the backup passes.
type Backup struct {
	DownloadAttachments bool `json:"download_attachments"`
	// BackfillAttachments downloads files missing on disk for messages that are
	// already archived (one-shot; message data untouched).
	BackfillAttachments bool `json:"backfill_attachments"`
	IncludeBots         bool `json:"include_bots"`
	// SkipEmpty skips channels with no access or zero messages (default true).
	SkipEmpty *bool `json:"skip_empty"`
	// EditWindow is how far back edits/deletes are re-verified ("0" disables).
	EditWindow string `json:"edit_window"`
	// AttachMaxMB caps a single attachment download.
	AttachMaxMB int `json:"attach_max_mb"`
	// AttachWorkers is the number of parallel attachment downloads.
	AttachWorkers int `json:"attach_workers"`
	// Every turns the backup into its own scheduler: wait this long after each
	// pass, then run again ("0" or "" = run once). This is what lets a single
	// container be a backup service without cron.
	Every string `json:"every"`
	// Limit caps the initial backfill per channel/thread (newest N, 0 =
	// unlimited); mutually exclusive with Since.
	Limit int `json:"limit"`
	// Since is the first-run lower bound (RFC3339 or YYYY-MM-DD).
	Since string `json:"since"`
	// Channels restricts the archive to these channels/threads (name or ID).
	Channels []string `json:"channels"`
}

// MCP controls the read-only MCP server.
type MCP struct {
	// HTTPAddr is the default listen address for "discord-backup mcp --http".
	HTTPAddr string `json:"http_addr"`
	// DefaultPrincipal is the identity used by the stdio transport when no
	// --principal is given (and more than one principal is configured).
	DefaultPrincipal string `json:"default_principal"`
	// Deleted is "hide" (default) or "show".
	Deleted string `json:"deleted"`
	// Cache lifetimes and outage behaviour (Go duration strings).
	RefreshTTL string `json:"refresh_ttl"` // archive rescan interval (default 60s)
	ChannelTTL string `json:"channel_ttl"` // channel+role cache (default 5m)
	MemberTTL  string `json:"member_ttl"`  // member roles cache (default 60s)
	RetryTTL   string `json:"retry_ttl"`   // pause live lookups after a failure (default 5m)
	Timeout    string `json:"timeout"`     // per live lookup (default 5s)
	// CheckPrivateThreads verifies private-thread membership live (extra API
	// calls); off by default, which keeps private threads hidden.
	CheckPrivateThreads bool `json:"check_private_threads"`
	// AuditLog is an optional JSONL file receiving one line per tool call.
	AuditLog string `json:"audit_log"`
	// Principals maps people to Discord user IDs; the MCP server refuses to
	// start without at least one.
	Principals []Principal `json:"principals"`
}

// Principal maps one human (by name, email, Discord ID or alias) to the
// Discord user ID whose permissions gate everything they can see.
type Principal struct {
	Name      string   `json:"name"`
	DiscordID string   `json:"discord_id"`
	Email     string   `json:"email,omitempty"`
	Aliases   []string `json:"aliases,omitempty"`
	// Token is the bearer token the HTTP transport requires for this
	// principal. Required for HTTP, unique per principal.
	// omitempty keeps "discord-backup principals" output clean: an empty
	// token simply does not show up in the pasted or written config.
	Token string `json:"token,omitempty"`
}

// Options is the configuration with defaults applied, durations parsed and
// dates resolved – what the commands actually run with.
type Options struct {
	GuildID    string
	ArchiveDir string
	Token      string
	Backup     BackupOptions
	MCP        MCPOptions
}

// BackupOptions are the parsed backup settings.
type BackupOptions struct {
	DownloadAttachments bool
	BackfillAttachments bool
	IncludeBots         bool
	SkipEmpty           bool
	EditWindow          time.Duration
	AttachMaxMB         int
	AttachWorkers       int
	Every               time.Duration
	Limit               int
	Since               time.Time
	Channels            []string
}

// MCPOptions are the parsed MCP settings.
type MCPOptions struct {
	HTTPAddr            string
	DefaultPrincipal    string
	Deleted             string // "hide" (default) or "show"
	ShowDeleted         bool
	RefreshTTL          time.Duration
	ChannelTTL          time.Duration
	MemberTTL           time.Duration
	RetryTTL            time.Duration
	Timeout             time.Duration
	CheckPrivateThreads bool
	AuditLog            string
	Principals          []Principal
}

// Load reads and validates the configuration file.
//
// Both permission failures are explained in the error, because in a container
// the usual cause is ownership rather than the mode: the shipped image runs as
// uid 65532, so a 0600 file owned by root is simply unreadable.
func Load(path string) (*Options, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%s is a directory, want the configuration file", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, readErr(path, err)
	}
	c, opts, err := parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.hasSecrets() {
		if err := checkSecretFilePerms(path, fi.Mode().Perm()); err != nil {
			return nil, err
		}
	}
	return opts, nil
}

// readErr turns "permission denied" into something actionable: it names the uid
// the process runs as and the chown that fixes it.
func readErr(path string, err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("read %s: permission denied as uid %d (the file must be readable/owned by that user; in a container: sudo chown %d:%d %s)",
			path, os.Geteuid(), os.Geteuid(), os.Getegid(), path)
	}
	return fmt.Errorf("read %s: %w", path, err)
}

// Parse validates a configuration document without touching the filesystem
// (used to keep the shipped example honest).
func Parse(raw []byte) (*Options, error) {
	_, opts, err := parse(raw)
	return opts, err
}

func parse(raw []byte) (*Config, *Options, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, nil, fmt.Errorf("parse: %w", err)
	}
	opts, err := c.options()
	if err != nil {
		return nil, nil, err
	}
	return &c, opts, nil
}

// validate checks the whole document: shared fields and values that are
// malformed no matter which command runs (a required key, a negative number, an
// unparseable duration). Requirements that belong to one command only live in
// CheckBackup / CheckMCP instead, so a problem in the "mcp" section cannot stop
// the backup.
func (c *Config) validate() error {
	if strings.TrimSpace(c.ArchiveDir) == "" {
		return fmt.Errorf("archive_dir is required")
	}
	if strings.TrimSpace(c.GuildID) == "" {
		return fmt.Errorf("guild_id is required")
	}
	if c.Backup.Limit < 0 {
		return fmt.Errorf("backup.limit must be >= 0")
	}
	if c.Backup.AttachMaxMB < 0 {
		return fmt.Errorf("backup.attach_max_mb must be >= 0")
	}
	if c.Backup.AttachWorkers < 0 {
		return fmt.Errorf("backup.attach_workers must be >= 0")
	}
	return nil
}

// CheckBackup reports requirements only the backup command has.
func (o *Options) CheckBackup() error {
	if o.Backup.Limit > 0 && !o.Backup.Since.IsZero() {
		return fmt.Errorf("backup.limit and backup.since are mutually exclusive")
	}
	return nil
}

// CheckMCP reports requirements only the MCP command has: at least one
// principal, a sane deleted mode, unique tokens, and a bearer token whenever an
// HTTP listen address is configured. None of this can stop the backup.
func (o *Options) CheckMCP() error {
	switch o.MCP.Deleted {
	case "hide", "show":
	default:
		return fmt.Errorf(`mcp.deleted must be "hide" or "show"`)
	}
	if len(o.MCP.Principals) == 0 {
		return fmt.Errorf(`no principals configured: add "mcp.principals"`)
	}
	seen := map[string]bool{}
	tokens := map[string]string{}
	for i := range o.MCP.Principals {
		p := &o.MCP.Principals[i]
		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("mcp.principals[%d]: name is required", i)
		}
		key := strings.ToLower(p.Name)
		if seen[key] {
			return fmt.Errorf("duplicate principal name %q", p.Name)
		}
		seen[key] = true
		if strings.TrimSpace(p.DiscordID) == "" {
			return fmt.Errorf("mcp.principals[%d] (%s): discord_id is required", i, p.Name)
		}
		if p.Token != "" {
			if other, dup := tokens[p.Token]; dup {
				return fmt.Errorf("principals %q and %q share the same token", other, p.Name)
			}
			tokens[p.Token] = p.Name
		}
	}
	if o.MCP.HTTPAddr == "" || len(tokens) > 0 {
		return nil
	}
	return fmt.Errorf(`mcp.http_addr is set but no principal has a "token"`)
}

func (c *Config) options() (*Options, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	dur := func(field, val string, def time.Duration) (time.Duration, error) {
		if strings.TrimSpace(val) == "" {
			return def, nil
		}
		d, err := time.ParseDuration(val)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", field, err)
		}
		return d, nil
	}
	o := &Options{
		GuildID:    strings.TrimSpace(c.GuildID),
		ArchiveDir: strings.TrimSpace(c.ArchiveDir),
		Token:      strings.TrimSpace(c.Token),
	}
	b := &o.Backup
	b.DownloadAttachments = c.Backup.DownloadAttachments
	b.BackfillAttachments = c.Backup.BackfillAttachments
	b.IncludeBots = c.Backup.IncludeBots
	b.SkipEmpty = c.Backup.SkipEmpty == nil || *c.Backup.SkipEmpty
	b.AttachMaxMB = c.Backup.AttachMaxMB
	b.AttachWorkers = c.Backup.AttachWorkers
	if b.AttachMaxMB == 0 {
		b.AttachMaxMB = 50
	}
	if b.AttachWorkers == 0 {
		b.AttachWorkers = 4
	}
	b.Limit = c.Backup.Limit
	b.Channels = c.Backup.Channels
	var err error
	if b.EditWindow, err = dur("backup.edit_window", c.Backup.EditWindow, 48*time.Hour); err != nil {
		return nil, err
	}
	if b.Every, err = dur("backup.every", c.Backup.Every, 0); err != nil {
		return nil, err
	}
	if strings.TrimSpace(c.Backup.Since) != "" {
		if b.Since, err = parseDate(c.Backup.Since); err != nil {
			return nil, fmt.Errorf("backup.since: %w", err)
		}
	}

	m := &o.MCP
	m.HTTPAddr = strings.TrimSpace(c.MCP.HTTPAddr)
	m.DefaultPrincipal = strings.TrimSpace(c.MCP.DefaultPrincipal)
	m.Deleted = "hide"
	if c.MCP.Deleted != "" {
		m.Deleted = c.MCP.Deleted
	}
	m.ShowDeleted = m.Deleted == "show"
	m.CheckPrivateThreads = c.MCP.CheckPrivateThreads
	m.AuditLog = strings.TrimSpace(c.MCP.AuditLog)
	m.Principals = c.MCP.Principals
	if m.RefreshTTL, err = dur("mcp.refresh_ttl", c.MCP.RefreshTTL, time.Minute); err != nil {
		return nil, err
	}
	if m.ChannelTTL, err = dur("mcp.channel_ttl", c.MCP.ChannelTTL, 5*time.Minute); err != nil {
		return nil, err
	}
	if m.MemberTTL, err = dur("mcp.member_ttl", c.MCP.MemberTTL, time.Minute); err != nil {
		return nil, err
	}
	if m.RetryTTL, err = dur("mcp.retry_ttl", c.MCP.RetryTTL, 5*time.Minute); err != nil {
		return nil, err
	}
	if m.Timeout, err = dur("mcp.timeout", c.MCP.Timeout, 5*time.Second); err != nil {
		return nil, err
	}
	return o, nil
}

// parseDate accepts an RFC3339 timestamp or a plain YYYY-MM-DD date
// (midnight UTC).
func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("parse %q: want RFC3339 (2026-01-01T00:00:00Z) or YYYY-MM-DD", s)
}

// ResolvePrincipal finds a principal by name, email, Discord ID or alias
// (case-insensitive).
func ResolvePrincipal(principals []Principal, identifier string) (*Principal, error) {
	id := strings.ToLower(strings.TrimSpace(identifier))
	if id == "" {
		return nil, fmt.Errorf("empty principal identifier")
	}
	for i := range principals {
		p := &principals[i]
		if strings.ToLower(p.Name) == id || strings.ToLower(p.Email) == id || p.DiscordID == strings.TrimSpace(identifier) {
			return p, nil
		}
		for _, a := range p.Aliases {
			if strings.ToLower(a) == id {
				return p, nil
			}
		}
	}
	names := make([]string, 0, len(principals))
	for _, p := range principals {
		names = append(names, p.Name)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no principals configured")
	}
	return nil, fmt.Errorf("unknown principal %q (known: %s)", identifier, strings.Join(names, ", "))
}

// PrincipalByToken finds a principal by HTTP bearer token (constant time).
func PrincipalByToken(principals []Principal, token string) (*Principal, bool) {
	if token == "" {
		return nil, false
	}
	for i := range principals {
		if principals[i].Token != "" && constantTimeEqual(principals[i].Token, token) {
			return &principals[i], true
		}
	}
	return nil, false
}

// hasSecrets reports whether the config carries a secret: the bot token or any
// principal's HTTP bearer token.
func (c *Config) hasSecrets() bool {
	if strings.TrimSpace(c.Token) != "" {
		return true
	}
	for i := range c.MCP.Principals {
		if strings.TrimSpace(c.MCP.Principals[i].Token) != "" {
			return true
		}
	}
	return false
}

// checkSecretFilePerms refuses a secret-bearing config that others can read, and
// names the ownership trap on the way (a 0600 file owned by another user is
// unreadable inside a container).
func checkSecretFilePerms(path string, perm fs.FileMode) error {
	if perm&0o077 != 0 {
		return fmt.Errorf("config %s holds tokens and must not be group/world accessible (chmod 600 %s); the process (uid %d) must own it – in a container: sudo chown %d:%d %s",
			path, path, os.Geteuid(), os.Geteuid(), os.Getegid(), path)
	}
	return nil
}

// constantTimeEqual is a constant-time string comparison.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
