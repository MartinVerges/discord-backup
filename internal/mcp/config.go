package mcp

import (
	"fmt"
	"strings"
	"time"

	"discord-backup/internal/config"
)

// Settings is one MCP server instance's view of the shared configuration file
// (internal/config): the same file the backup reads.
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
	Principals          []config.Principal
}

// Resolve finds a principal by name, email, Discord ID or alias
// (case-insensitive).
func (s Settings) Resolve(identifier string) (*config.Principal, error) {
	return config.ResolvePrincipal(s.Principals, identifier)
}

// ByToken finds a principal by HTTP bearer token (constant time).
func (s Settings) ByToken(token string) (*config.Principal, bool) {
	return config.PrincipalByToken(s.Principals, token)
}

// StdioPrincipal resolves the principal for the stdio transport from an
// optional --principal value, falling back to default_principal and then to
// the only configured principal.
func (s Settings) StdioPrincipal(flagValue string) (*config.Principal, error) {
	if strings.TrimSpace(flagValue) != "" {
		return s.Resolve(flagValue)
	}
	if s.DefaultPrincipal != "" {
		return s.Resolve(s.DefaultPrincipal)
	}
	switch len(s.Principals) {
	case 0:
		return nil, fmt.Errorf(`no principals configured (add "mcp.principals" to the config)`)
	case 1:
		return &s.Principals[0], nil
	}
	return nil, fmt.Errorf("multiple principals configured: pass --principal <name|email|discord_id> or set default_principal")
}
