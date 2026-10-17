package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"discord-backup/internal/archive"
)

// toolDefs returns the MCP tool catalogue.
func toolDefs() []map[string]any {
	return []map[string]any{
		{
			"name": "whoami",
			"description": "Identify the caller and report exactly which archived channels, " +
				"threads and messages are visible. Call this first.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "list_channels",
			"description": "List the archived channels (and optionally threads) the caller may read.",
			"inputSchema": object(map[string]any{
				"include_threads": boolProp("also list threads (default false)"),
				"include_empty":   boolProp("include channels with no archived messages (default false)"),
			}),
		},
		{
			"name":        "list_threads",
			"description": "List the threads of one channel that the caller may read.",
			"inputSchema": object(map[string]any{
				"channel": strProp("channel name (#name also works) or channel ID"),
			}, "channel"),
		},
		{
			"name": "search_messages",
			"description": "Full-text search across every message the caller may read. " +
				"All query terms must match (case-insensitive substring); terms also match " +
				"attachment filenames. With no query it returns the most recent messages.",
			"inputSchema": object(map[string]any{
				"query":   strProp("search terms; all must match"),
				"channel": strProp("restrict to a channel/thread name or ID"),
				"author":  strProp("restrict to an author username or user ID"),
				"after":   strProp("only messages at/after this time (RFC3339 or YYYY-MM-DD)"),
				"before":  strProp("only messages before this time"),
				"limit":   intProp("max results (default 50, max 500)"),
				"order":   enumProp("newest or oldest (default newest)", "newest", "oldest"),
			}),
		},
		{
			"name":        "get_message",
			"description": "Fetch one message by ID, including its full raw JSON payload.",
			"inputSchema": object(map[string]any{
				"message_id": strProp("Discord message ID"),
			}, "message_id"),
		},
		{
			"name":        "get_context",
			"description": "Fetch the messages around one message within the same channel or thread.",
			"inputSchema": object(map[string]any{
				"message_id": strProp("Discord message ID"),
				"before":     intProp("messages before (default 10)"),
				"after":      intProp("messages after (default 10)"),
			}, "message_id"),
		},
		{
			"name":        "list_authors",
			"description": "List the most active authors among the messages the caller may read, optionally filtered by terms.",
			"inputSchema": object(map[string]any{
				"query": strProp("optional search terms to filter messages"),
				"limit": intProp("max authors (default 20)"),
			}),
		},
	}
}

func object(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func enumProp(desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": vals}
}

// runTool executes a tool for a principal and returns (result, resultCount, error).
func (s *Server) runTool(ctx context.Context, principal *Principal, name string, args json.RawMessage) (any, int, error) {
	allowed, access, err := s.accessFor(ctx, principal)
	if err != nil {
		return nil, 0, err
	}
	switch name {
	case "whoami":
		return s.toolWhoami(principal, allowed, access), 0, nil
	case "list_channels":
		return s.toolListChannels(args, access)
	case "list_threads":
		return s.toolListThreads(args, access)
	case "search_messages":
		return s.toolSearch(args, access)
	case "get_message":
		return s.toolGetMessage(args, access)
	case "get_context":
		return s.toolGetContext(args, access)
	case "list_authors":
		return s.toolListAuthors(args, access)
	default:
		return nil, 0, fmt.Errorf("unknown tool %q", name)
	}
}

// accessFor computes the caller's readable channel set and the matching
// archive access predicate.
func (s *Server) accessFor(ctx context.Context, principal *Principal) (map[string]bool, archive.Access, error) {
	allowed, err := s.res.AllowedChannels(ctx, principal.DiscordID)
	if err != nil {
		return nil, nil, err
	}
	var threadAllowed func(archive.LogInfo) bool
	if s.settings.CheckPrivateThreads && s.client != nil {
		threadAllowed = s.threadMembership(ctx, principal.DiscordID, allowed)
	}
	return allowed, archive.ChannelAccess(allowed, threadAllowed), nil
}

// threadMembership verifies private-thread membership live, with a per-call
// cache. Fail-closed: any error means "not a member".
func (s *Server) threadMembership(ctx context.Context, userID string, allowed map[string]bool) func(archive.LogInfo) bool {
	var mu sync.Mutex
	cache := map[string]bool{}
	return func(l archive.LogInfo) bool {
		if !allowed[l.ParentID] {
			return false
		}
		mu.Lock()
		v, ok := cache[l.ID]
		mu.Unlock()
		if ok {
			return v
		}
		cctx, cancel := context.WithTimeout(ctx, s.settings.Timeout)
		member, err := s.client.ThreadMember(cctx, l.ID, userID)
		cancel()
		if err != nil {
			s.log.Warn("thread membership check failed, denying", "thread", l.ID, "err", err)
			member = false
		}
		mu.Lock()
		cache[l.ID] = member
		mu.Unlock()
		return member
	}
}

// --- individual tools ------------------------------------------------------

func (s *Server) toolWhoami(principal *Principal, allowed map[string]bool, access archive.Access) map[string]any {
	logs, messages := s.arch.Count(access)
	channels, threads, untyped := 0, 0, 0
	for _, l := range s.arch.LogsInfo(access) {
		if l.Kind == archive.KindThread {
			threads++
			if l.Type == 0 {
				untyped++
			}
		} else {
			channels++
		}
	}
	st := s.res.Status()
	source := "snapshot"
	note := "Using the permission snapshot captured by the backup."
	if s.client == nil {
		source = "snapshot-only"
		note = "No bot token configured: permissions come solely from the archive snapshot."
	} else if st.LiveChannels {
		source = "live"
		note = "Permissions refreshed live; the archive snapshot is the fallback when Discord is unreachable."
	}
	if untyped > 0 {
		note += fmt.Sprintf(" %d thread(s) have no recorded type (archive written by an older version) and are hidden; run the backup once to record thread types.", untyped)
	}
	deleted := "hidden"
	if s.settings.ShowDeleted {
		deleted = "visible (marked)"
	}
	return map[string]any{
		"principal":             principal.Name,
		"discord_id":            principal.DiscordID,
		"email":                 principal.Email,
		"guild":                 map[string]any{"id": s.arch.GuildID(), "name": s.arch.Name()},
		"permission_source":     source,
		"snapshot_fetched_at":   st.SnapshotFetched,
		"live_lookups_failed":   st.LiveFails,
		"deleted_messages":      deleted,
		"private_thread_access": s.settings.CheckPrivateThreads,
		"visible_channels":      channels,
		"visible_threads":       threads,
		"threads_without_type":  untyped,
		"visible_logs":          logs,
		"visible_messages":      messages,
		"note":                  note,
	}
}

func (s *Server) toolListChannels(args json.RawMessage, access archive.Access) (any, int, error) {
	var a struct {
		IncludeThreads bool `json:"include_threads"`
		IncludeEmpty   bool `json:"include_empty"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, 0, err
	}
	threadCount := map[string]int{}
	var chans []archive.LogInfo
	for _, l := range s.arch.LogsInfo(access) {
		if l.Kind == archive.KindThread {
			threadCount[l.ParentID]++
			continue
		}
		if l.NoAccess && !a.IncludeEmpty {
			continue
		}
		if l.Messages == 0 && !a.IncludeEmpty {
			continue
		}
		chans = append(chans, l)
	}
	sort.Slice(chans, func(i, j int) bool {
		if chans[i].Category != chans[j].Category {
			return chans[i].Category < chans[j].Category
		}
		return chans[i].Name < chans[j].Name
	})
	out := make([]map[string]any, 0, len(chans))
	for _, l := range chans {
		out = append(out, channelView(l, threadCount[l.ID]))
	}
	if a.IncludeThreads {
		var threads []archive.LogInfo
		for _, l := range s.arch.LogsInfo(access) {
			if l.Kind == archive.KindThread && !l.NoAccess {
				threads = append(threads, l)
			}
		}
		sort.Slice(threads, func(i, j int) bool { return threads[i].Name < threads[j].Name })
		for _, l := range threads {
			out = append(out, channelView(l, 0))
		}
	}
	return map[string]any{"channels": out, "count": len(out)}, len(out), nil
}

func channelView(l archive.LogInfo, threads int) map[string]any {
	v := map[string]any{
		"id":       l.ID,
		"name":     l.Name,
		"kind":     string(l.Kind),
		"type":     l.Type,
		"category": l.Category,
		"messages": l.Messages,
	}
	if l.Kind == archive.KindThread {
		v["parent_id"] = l.ParentID
		v["archived"] = l.Archived
		v["locked"] = l.Locked
	} else if threads > 0 {
		v["thread_count"] = threads
	}
	return v
}

func (s *Server) toolListThreads(args json.RawMessage, access archive.Access) (any, int, error) {
	var a struct {
		Channel string `json:"channel"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, 0, err
	}
	if strings.TrimSpace(a.Channel) == "" {
		return nil, 0, fmt.Errorf("channel is required (name or ID)")
	}
	ch, ok := s.findChannel(a.Channel, access)
	if !ok {
		return nil, 0, fmt.Errorf("channel %q not found or not visible", a.Channel)
	}
	var out []map[string]any
	for _, l := range s.arch.LogsInfo(access) {
		if l.Kind == archive.KindThread && l.ParentID == ch.ID && !l.NoAccess {
			out = append(out, channelView(l, 0))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	return map[string]any{"channel": channelView(ch, len(out)), "threads": out, "count": len(out)}, len(out), nil
}

// findChannel resolves a channel name or ID among the visible channels.
func (s *Server) findChannel(name string, access archive.Access) (archive.LogInfo, bool) {
	name = trimChannelName(name)
	if l, ok := s.arch.LogInfoByID(name, access); ok && l.Kind == archive.KindChannel {
		return l, true
	}
	lower := strings.ToLower(name)
	for _, l := range s.arch.LogsInfo(access) {
		if l.Kind == archive.KindChannel && strings.ToLower(l.Name) == lower {
			return l, true
		}
	}
	for _, l := range s.arch.LogsInfo(access) {
		if l.Kind == archive.KindChannel && strings.Contains(strings.ToLower(l.Name), lower) {
			return l, true
		}
	}
	return archive.LogInfo{}, false
}

func (s *Server) toolSearch(args json.RawMessage, access archive.Access) (any, int, error) {
	var a struct {
		Query   string `json:"query"`
		Channel string `json:"channel"`
		Author  string `json:"author"`
		After   string `json:"after"`
		Before  string `json:"before"`
		Limit   int    `json:"limit"`
		Order   string `json:"order"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, 0, err
	}
	after, err := parseTime(a.After)
	if err != nil {
		return nil, 0, err
	}
	before, err := parseTime(a.Before)
	if err != nil {
		return nil, 0, err
	}
	q := archive.Query{
		Terms:   strings.Fields(a.Query),
		Channel: trimChannelName(a.Channel),
		Author:  strings.TrimSpace(a.Author),
		After:   after,
		Before:  before,
		Limit:   a.Limit,
		Oldest:  strings.EqualFold(a.Order, "oldest"),
	}
	if hit, ok := s.arch.LogInfoByID(trimChannelName(a.Channel), access); ok {
		// Exact ID: use the precise ID (names are matched case-insensitively
		// anyway, and an exact match avoids partial-name surprises).
		q.Channel = hit.ID
	}
	hits, total := s.arch.Search(access, q)
	return map[string]any{
		"total":   total,
		"count":   len(hits),
		"results": hits,
		"note":    noteFor(q, hits, total),
	}, len(hits), nil
}

func noteFor(q archive.Query, hits []archive.Hit, total int) string {
	if total > len(hits) {
		return fmt.Sprintf("Showing %d of %d matches; raise limit or narrow the query.", len(hits), total)
	}
	if total == 0 {
		return "No visible messages match. This can mean the content is not archived, or it is outside the caller's Discord permissions."
	}
	return ""
}

func (s *Server) toolGetMessage(args json.RawMessage, access archive.Access) (any, int, error) {
	var a struct {
		MessageID string `json:"message_id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, 0, err
	}
	if strings.TrimSpace(a.MessageID) == "" {
		return nil, 0, fmt.Errorf("message_id is required")
	}
	d, raw, ok := s.arch.Message(strings.TrimSpace(a.MessageID), access)
	if !ok {
		return nil, 0, fmt.Errorf("message %s not found or not visible to this principal", a.MessageID)
	}
	out := map[string]any{"message": messageView(d)}
	if len(raw) > 0 {
		out["raw"] = json.RawMessage(raw)
	}
	return out, 1, nil
}

func (s *Server) toolGetContext(args json.RawMessage, access archive.Access) (any, int, error) {
	var a struct {
		MessageID string `json:"message_id"`
		Before    int    `json:"before"`
		After     int    `json:"after"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, 0, err
	}
	if strings.TrimSpace(a.MessageID) == "" {
		return nil, 0, fmt.Errorf("message_id is required")
	}
	before, after := a.Before, a.After
	if before <= 0 {
		before = 10
	}
	if after <= 0 {
		after = 10
	}
	if before > 100 {
		before = 100
	}
	if after > 100 {
		after = 100
	}
	docs, logInfo, ok := s.arch.MessageContext(strings.TrimSpace(a.MessageID), access, before, after)
	if !ok {
		return nil, 0, fmt.Errorf("message %s not found or not visible to this principal", a.MessageID)
	}
	views := make([]map[string]any, 0, len(docs))
	for _, d := range docs {
		views = append(views, messageView(d))
	}
	return map[string]any{
		"channel":    logInfo.Name,
		"channel_id": logInfo.ID,
		"kind":       string(logInfo.Kind),
		"count":      len(views),
		"messages":   views,
	}, len(views), nil
}

func (s *Server) toolListAuthors(args json.RawMessage, access archive.Access) (any, int, error) {
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, 0, err
	}
	counts := s.arch.Authors(access, strings.Fields(a.Query), a.Limit)
	return map[string]any{"authors": counts, "count": len(counts)}, len(counts), nil
}

// --- helpers ---------------------------------------------------------------

func messageView(d archive.MessageInfo) map[string]any {
	v := map[string]any{
		"id":         d.ID,
		"channel":    d.Channel,
		"channel_id": d.ChannelID,
		"kind":       string(d.Kind),
		"author":     d.Author,
		"author_id":  d.AuthorID,
		"timestamp":  d.TS,
		"content":    d.Content,
	}
	if d.Edited {
		v["edited"] = true
	}
	if d.Deleted {
		v["deleted"] = true
	}
	if d.Attach {
		v["has_attachments"] = true
	}
	return v
}

func decodeArgs(raw json.RawMessage, out any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time %q (use RFC3339 or YYYY-MM-DD)", s)
}
