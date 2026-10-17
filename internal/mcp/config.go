package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is the on-disk configuration for "discord-backup mcp". It is JSON,
// which keeps the tool dependency-free; see docs/mcp.md for a full example.
type Config struct {
	// ArchiveDir is the backup "out" directory (containing server_<id>).
	ArchiveDir string `json:"archive_dir"`
	// GuildID selects one archive when ArchiveDir holds several.
	GuildID string `json:"guild_id"`

	// Token (inline) enables live permission lookups. It is the same bot token
	// the backup uses; without it the server runs purely on the archive's
	// permissions snapshot. Keeping it in this file is the normal case, which
	// is why a config that holds any token must be mode 0600. TokenFile is the
	// alternative for injecting the secret separately (Docker secrets, systemd
	// credentials); it must be 0600 on its own.
	Token     string `json:"token"`
	TokenFile string `json:"token_file"`

	// Deleted is "hide" (default) or "show".
	Deleted string `json:"deleted"`

	// Cache lifetimes (Go duration strings).
	RefreshTTL string `json:"refresh_ttl"` // archive rescan interval (default 60s)
	ChannelTTL string `json:"channel_ttl"` // channel+role cache (default 5m)
	MemberTTL  string `json:"member_ttl"`  // member roles cache (default 60s)
	RetryTTL   string `json:"retry_ttl"`   // pause live lookups after a failure (default 5m)
	Timeout    string `json:"timeout"`     // per live lookup (default 5s)

	// CheckPrivateThreads enables a live thread-membership lookup so private
	// threads a user belongs to become readable. Off by default: private
	// threads stay hidden, the safe choice that costs no extra API calls.
	CheckPrivateThreads bool `json:"check_private_threads"`

	// AuditLog is an optional JSONL file receiving one line per tool call.
	AuditLog string `json:"audit_log"`

	// DefaultPrincipal is used by the stdio transport when no --principal is
	// given (and there is more than one principal).
	DefaultPrincipal string `json:"default_principal"`

	// HTTPAddr is the default listen address for the HTTP transport.
	HTTPAddr string `json:"http_addr"`

	Principals []Principal `json:"principals"`
}

// Principal maps one human (by name, email, Discord ID or alias) to a
// Discord user ID whose permissions gate everything they can see.
type Principal struct {
	Name      string   `json:"name"`
	DiscordID string   `json:"discord_id"`
	Email     string   `json:"email"`
	Aliases   []string `json:"aliases"`
	// Token is the bearer token for the HTTP transport. Required for HTTP.
	Token string `json:"token"`
}

// LoadConfig reads and validates the config file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.hasSecrets() {
		if err := checkSecretFilePerms(path); err != nil {
			return nil, err
		}
	}
	return &c, nil
}

// hasSecrets reports whether the config carries a secret: the bot token or any
// principal's HTTP bearer token.
func (c *Config) hasSecrets() bool {
	if strings.TrimSpace(c.Token) != "" {
		return true
	}
	for i := range c.Principals {
		if strings.TrimSpace(c.Principals[i].Token) != "" {
			return true
		}
	}
	return false
}

// checkSecretFilePerms refuses a secret-bearing config that others can read, so
// an inline token is no looser than a token file.
func checkSecretFilePerms(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("config %s holds tokens and must not be group/world accessible (run: chmod 600 %s)", path, path)
	}
	return nil
}

func (c *Config) validate() error {
	if c.ArchiveDir == "" {
		return fmt.Errorf("archive_dir is required")
	}
	if len(c.Principals) == 0 {
		return fmt.Errorf("at least one principal is required")
	}
	seen := map[string]bool{}
	tokens := map[string]string{}
	for i := range c.Principals {
		p := &c.Principals[i]
		if p.Name == "" {
			return fmt.Errorf("principal %d: name is required", i)
		}
		if p.DiscordID == "" {
			return fmt.Errorf("principal %q: discord_id is required", p.Name)
		}
		key := strings.ToLower(p.Name)
		if seen[key] {
			return fmt.Errorf("duplicate principal name %q", p.Name)
		}
		seen[key] = true
		if p.Token != "" {
			if other, dup := tokens[p.Token]; dup {
				return fmt.Errorf("principals %q and %q share the same token", other, p.Name)
			}
			tokens[p.Token] = p.Name
		}
	}
	switch c.Deleted {
	case "", "hide", "show":
	default:
		return fmt.Errorf(`deleted must be "hide" or "show"`)
	}
	if c.HTTPAddr != "" && len(tokens) == 0 {
		return fmt.Errorf("http_addr is set but no principal has a %q", "token")
	}
	return nil
}

// Settings holds the config with defaults and parsed durations applied, and
// the token resolved.
type Settings struct {
	ArchiveDir          string
	GuildID             string
	Token               string
	ShowDeleted         bool
	RefreshTTL          time.Duration
	ChannelTTL          time.Duration
	MemberTTL           time.Duration
	RetryTTL            time.Duration
	Timeout             time.Duration
	CheckPrivateThreads bool
	AuditLog            string
	DefaultPrincipal    string
	HTTPAddr            string
	Principals          []Principal
}

// Settings validates the config, applies defaults and resolves the token.
func (c *Config) Settings() (Settings, error) {
	s := Settings{
		ArchiveDir:          c.ArchiveDir,
		GuildID:             c.GuildID,
		ShowDeleted:         c.Deleted == "show",
		CheckPrivateThreads: c.CheckPrivateThreads,
		AuditLog:            c.AuditLog,
		DefaultPrincipal:    c.DefaultPrincipal,
		HTTPAddr:            c.HTTPAddr,
		Principals:          c.Principals,
		Token:               strings.TrimSpace(c.Token),
	}
	if s.Token == "" && c.TokenFile != "" {
		tok, err := readTokenFile(c.TokenFile)
		if err != nil {
			return s, err
		}
		s.Token = tok
	}
	dur := func(field, val string, def time.Duration) (time.Duration, error) {
		if val == "" {
			return def, nil
		}
		d, err := time.ParseDuration(val)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", field, err)
		}
		return d, nil
	}
	var err error
	if s.RefreshTTL, err = dur("refresh_ttl", c.RefreshTTL, time.Minute); err != nil {
		return s, err
	}
	if s.ChannelTTL, err = dur("channel_ttl", c.ChannelTTL, 5*time.Minute); err != nil {
		return s, err
	}
	if s.MemberTTL, err = dur("member_ttl", c.MemberTTL, time.Minute); err != nil {
		return s, err
	}
	if s.RetryTTL, err = dur("retry_ttl", c.RetryTTL, 5*time.Minute); err != nil {
		return s, err
	}
	if s.Timeout, err = dur("timeout", c.Timeout, 5*time.Second); err != nil {
		return s, err
	}
	return s, nil
}

// readTokenFile reads a bot token, refusing group/world-accessible files.
func readTokenFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("token file %s must not be group/world accessible (run: chmod 600 %s)", path, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return tok, nil
}

// Resolve finds a principal by name, email, Discord ID or alias
// (case-insensitive).
func resolvePrincipal(principals []Principal, identifier string) (*Principal, error) {
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
	return nil, fmt.Errorf("unknown principal %q (known: %s)", identifier, strings.Join(names, ", "))
}

// Resolve finds a principal in the settings.
func (s Settings) Resolve(identifier string) (*Principal, error) {
	return resolvePrincipal(s.Principals, identifier)
}

// ByToken finds a principal by HTTP bearer token (constant time).
func (s Settings) ByToken(token string) (*Principal, bool) {
	if token == "" {
		return nil, false
	}
	for i := range s.Principals {
		if s.Principals[i].Token != "" && subtleEqual(s.Principals[i].Token, token) {
			return &s.Principals[i], true
		}
	}
	return nil, false
}

// StdioPrincipal resolves the principal for the stdio transport from an
// optional --principal value, falling back to default_principal and then to
// the only configured principal.
func (s Settings) StdioPrincipal(flagValue string) (*Principal, error) {
	if strings.TrimSpace(flagValue) != "" {
		return s.Resolve(flagValue)
	}
	if s.DefaultPrincipal != "" {
		return s.Resolve(s.DefaultPrincipal)
	}
	if len(s.Principals) == 1 {
		return &s.Principals[0], nil
	}
	return nil, fmt.Errorf("multiple principals configured: pass --principal <name|email|discord_id> or set default_principal")
}

// subtleEqual is a constant-time string comparison.
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
