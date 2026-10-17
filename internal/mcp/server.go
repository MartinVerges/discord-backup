package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"discord-backup/internal/archive"
	"discord-backup/internal/authz"
	"discord-backup/internal/config"
	"discord-backup/internal/discord"
	"discord-backup/internal/store"
)

// Version is what MCP clients see as the server version in `initialize`
// (and what `healthcheck --principal` prints). The command stamps the build
// version into it at start-up; a library user gets "dev".
var Version = "dev"

const (
	serverName             = "discord-backup"
	defaultProtocolVersion = "2025-06-18"
)

// auditWriter appends one JSON line per tool call.
type auditWriter struct {
	path string
	log  *slog.Logger
	mu   sync.Mutex
}

type auditEntry struct {
	TS        string          `json:"ts"`
	Principal string          `json:"principal"`
	DiscordID string          `json:"discord_id"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args,omitempty"`
	OK        bool            `json:"ok"`
	Count     int             `json:"count,omitempty"`
	Error     string          `json:"error,omitempty"`
}

func (w *auditWriter) write(e auditEntry) {
	if w == nil || w.path == "" {
		return
	}
	e.TS = time.Now().UTC().Format(time.RFC3339)
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := store.AppendLines(w.path, [][]byte{line}); err != nil && w.log != nil {
		w.log.Warn("audit log write failed", "path", w.path, "err", err)
	}
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Request is one JSON-RPC 2.0 request or notification.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether the message expects no response.
func (r *Request) IsNotification() bool { return len(r.ID) == 0 }

// Response is one JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Server answers MCP requests against one archive, scoped by principal.
type Server struct {
	settings Settings
	arch     *archive.Archive
	res      *authz.Resolver
	client   *discord.Client
	audit    *auditWriter
	log      *slog.Logger
}

// New opens the archive and prepares the resolver.
func New(ctx context.Context, settings Settings, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	arch, err := archive.Open(ctx, archive.Options{
		Dir:         settings.ArchiveDir,
		GuildID:     settings.GuildID,
		ShowDeleted: settings.ShowDeleted,
		RefreshTTL:  settings.RefreshTTL,
		Log:         log,
	})
	if err != nil {
		return nil, err
	}
	var client *discord.Client
	if settings.Token != "" {
		client = discord.New(settings.Token, "")
	}
	s := &Server{settings: settings, arch: arch, client: client, log: log}
	s.res = authz.New(authz.Config{
		Client:       client,
		SnapshotPath: snapshotPath(arch.Root()),
		GuildID:      firstNonEmpty(arch.GuildID(), settings.GuildID),
		ChannelTTL:   settings.ChannelTTL,
		MemberTTL:    settings.MemberTTL,
		RetryTTL:     settings.RetryTTL,
		Timeout:      settings.Timeout,
		Log:          log,
	})
	if settings.AuditLog != "" {
		if err := store.EnsureDir(filepath.Dir(settings.AuditLog)); err != nil {
			return nil, fmt.Errorf("audit log: %w", err)
		}
		s.audit = &auditWriter{path: settings.AuditLog, log: log}
	}
	return s, nil
}

// Handle dispatches one JSON-RPC message. It returns nil for notifications.
func (s *Server) Handle(ctx context.Context, principal *config.Principal, req *Request) *Response {
	if req == nil {
		return rpcErrReply(nil, -32600, "empty request")
	}
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return rpcErrReply(req.ID, -32600, `jsonrpc must be "2.0"`)
	}
	switch req.Method {
	case "initialize":
		return ok(req.ID, s.initialize(req.Params))
	case "notifications/initialized", "notifications/cancelled",
		"notifications/roots/list_changed", "notifications/progress":
		return nil
	case "ping":
		return ok(req.ID, map[string]any{})
	case "tools/list":
		return ok(req.ID, map[string]any{"tools": toolDefs()})
	case "tools/call":
		return s.callTool(ctx, principal, req)
	case "resources/list":
		return ok(req.ID, map[string]any{"resources": []any{}})
	case "resources/templates/list":
		return ok(req.ID, map[string]any{"resourceTemplates": []any{}})
	case "prompts/list":
		return ok(req.ID, map[string]any{"prompts": []any{}})
	default:
		return rpcErrReply(req.ID, -32601, fmt.Sprintf("method %q not found", req.Method))
	}
}

func (s *Server) initialize(params json.RawMessage) map[string]any {
	version := defaultProtocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err == nil && p.ProtocolVersion != "" {
			version = p.ProtocolVersion
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    serverName,
			"version": Version,
		},
		"instructions": "Read-only access to a Discord archive, restricted to what " +
			"the configured principal may read per Discord channel permissions. " +
			"Call whoami first to learn the visible scope, then search_messages / " +
			"list_channels / get_context. Content outside the principal's " +
			"permissions is never returned.",
	}
}

func (s *Server) callTool(ctx context.Context, principal *config.Principal, req *Request) *Response {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return ok(req.ID, toolError("invalid params: "+err.Error()))
		}
	}
	if principal == nil {
		return ok(req.ID, toolError("no principal resolved; configure a principal for this transport"))
	}

	s.arch.MaybeRefresh(ctx)

	start := time.Now()
	result, count, err := s.runTool(ctx, principal, p.Name, p.Arguments)
	entry := auditEntry{
		Principal: principal.Name,
		DiscordID: principal.DiscordID,
		Tool:      p.Name,
		Args:      p.Arguments,
		Count:     count,
	}
	if err != nil {
		entry.Error = err.Error()
		s.audit.write(entry)
		s.log.Info("tool call failed", "tool", p.Name, "principal", principal.Name, "err", err, "dur", time.Since(start))
		return ok(req.ID, toolError(err.Error()))
	}
	entry.OK = true
	s.audit.write(entry)
	s.log.Info("tool call", "tool", p.Name, "principal", principal.Name, "count", count, "dur", time.Since(start))
	return ok(req.ID, toolResult(result))
}

func ok(id json.RawMessage, result any) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Result: result}
}

func rpcErrReply(id json.RawMessage, code int, msg string) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// toolResult renders a successful tool call.
func toolResult(v any) map[string]any {
	text := ""
	if str, ok := v.(string); ok {
		text = str
	} else if raw, err := json.MarshalIndent(v, "", "  "); err == nil {
		text = string(raw)
	}
	out := map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": false,
	}
	if _, isStr := v.(string); !isStr {
		out["structuredContent"] = v
	}
	return out
}

// toolError renders a tool execution failure (not a protocol error).
func toolError(msg string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": msg}},
		"isError": true,
	}
}

func snapshotPath(root string) string { return filepath.Join(root, "permissions.json") }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func trimChannelName(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "#")
}
