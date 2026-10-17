// Package backup implements the incremental archive runner.
//
// Design (what makes repeated cron runs safe):
//   - The JSONL data file itself is the cursor. The run starts reading after
//     the last stored message ID, so there is no separate state file that
//     could disagree with the actual data (the duplicate bug of the
//     reference project).
//   - Lines are append-only and fsynced; a torn tail is truncated on start.
//   - An advisory lock file prevents overlapping runs.
//   - New IDs are asserted to be greater than the stored tip before append.
package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"discord-backup/internal/attach"
	"discord-backup/internal/discord"
	"discord-backup/internal/lock"
	"discord-backup/internal/perms"
	"discord-backup/internal/store"
)

// Config controls one backup run.
type Config struct {
	Token   string
	GuildID string
	APIBase string // empty = production API; set for tests
	OutDir  string
	Log     *slog.Logger

	IncludeBots         bool
	ChannelFilter       []string
	Limit               int       // initial backfill cap per log (newest N); 0 = unlimited
	Since               time.Time // first-run lower bound (zero = none); mutually exclusive with Limit
	BackfillAttachments bool      // one-shot: download attachments missing on disk for stored messages
	DownloadAttachments bool
	EditWindow          time.Duration // 0 disables edit/delete reconciliation
	AttachMaxBytes      int64
	AttachWorkers       int
	SkipEmpty           bool
	ListChannels        bool
}

// reconcileCap bounds the edit-window rescan per log per run. New messages
// are always fetched completely; only the (expensive) re-verification of
// old messages for edits/deletes is capped, with a warning when it hits.
const reconcileCap = 2000

// recentScanCap bounds local lines scanned for the window map.
const recentScanCap = 10000

// --- on-disk schema --------------------------------------------------------

// ServerInfo is server_{id}/server_info.json.
type ServerInfo struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Description     string `json:"description,omitempty"`
	Icon            string `json:"icon,omitempty"`
	MemberCount     int    `json:"member_count"`
	MemberCountMode string `json:"member_count_mode"` // "exact" or "approximate"
	ArchivedAt      string `json:"archived_at"`
	IncludeBots     bool   `json:"include_bots"`
	Tool            string `json:"tool"`
}

// ChannelInfo is channel_{id}/channel_info.json.
type ChannelInfo struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         int    `json:"type"`
	Topic        string `json:"topic,omitempty"`
	Position     int    `json:"position"`
	Category     string `json:"category,omitempty"`
	MessageCount int    `json:"message_count"`
	EditCount    int    `json:"edit_count"`
	DeleteCount  int    `json:"delete_count"`
	PinnedCount  int    `json:"pinned_count"`
	ThreadCount  int    `json:"thread_count"`
	NoAccess     bool   `json:"no_access,omitempty"`
	BackedUpAt   string `json:"backed_up_at"`
}

// ThreadInfo is thread_{id}/thread_info.json.
type ThreadInfo struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ParentID     string `json:"parent_id"`
	OwnerID      string `json:"owner_id,omitempty"`
	Type         int    `json:"type"` // 10 announcement, 11 public, 12 private thread
	Archived     bool   `json:"archived"`
	Locked       bool   `json:"locked"`
	MessageCount int    `json:"message_count"`
	EditCount    int    `json:"edit_count"`
	DeleteCount  int    `json:"delete_count"`
	NoAccess     bool   `json:"no_access,omitempty"`
	BackedUpAt   string `json:"backed_up_at"`
}

// ChannelIndex is one entry of channels.json.
type ChannelIndex struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         int    `json:"type"`
	Position     int    `json:"position"`
	Category     string `json:"category,omitempty"`
	MessageCount int    `json:"message_count"`
	ThreadCount  int    `json:"thread_count"`
	PinnedCount  int    `json:"pinned_count"`
	Status       string `json:"status"` // "ok" or "no_access"
}

// PinsSnapshot is pins.json.
type PinsSnapshot struct {
	FetchedAt  string   `json:"fetched_at"`
	MessageIDs []string `json:"message_ids"`
}

// DeleteMark is one line of deletes.jsonl.
type DeleteMark struct {
	ID         string `json:"id"`
	DetectedAt string `json:"detected_at"`
}

// --- runner ----------------------------------------------------------------

type Runner struct {
	cfg Config
	api *discord.Client
	log *slog.Logger
	dl  *attach.Downloader

	// Attachments downloaded inline during long backfills/heals (chunked
	// flushes), summed into the run totals at the end.
	attOK, attSkip, attFail int
}

func (c Config) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// Run executes one full incremental backup pass.
func Run(ctx context.Context, cfg Config) error {
	log := cfg.logger()
	if cfg.Token == "" {
		return errors.New(`no token (set "token" in the configuration file)`)
	}
	if cfg.GuildID == "" {
		return errors.New(`no guild (set "guild_id" in the configuration file)`)
	}
	if cfg.Limit > 0 && !cfg.Since.IsZero() {
		return errors.New("--limit and --since are mutually exclusive")
	}
	if err := store.EnsureDir(cfg.OutDir); err != nil {
		return fmt.Errorf("create out dir: %w", err)
	}
	l, err := lock.Acquire(filepath.Join(cfg.OutDir, ".lock"))
	if err != nil {
		if errors.Is(err, lock.ErrLocked) {
			// Not a failure: the previous cron run is simply still working.
			log.Info("another backup run is active, exiting")
			return nil
		}
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer l.Release()

	r := &Runner{
		cfg: cfg,
		api: discord.New(cfg.Token, cfg.APIBase),
		log: log,
		dl: &attach.Downloader{
			MaxBytes: cfg.AttachMaxBytes,
			Workers:  cfg.AttachWorkers,
			Log:      log,
		},
	}
	if r.cfg.BackfillAttachments {
		r.cfg.DownloadAttachments = true // backfill implies downloading
	}
	return r.run(ctx)
}

func (r *Runner) run(ctx context.Context) error {
	me, err := r.api.Me(ctx)
	if err != nil {
		return fmt.Errorf("validate token: %w", err)
	}
	r.log.Info("authenticated", "bot", me.Username)

	guild, err := r.api.Guild(ctx, r.cfg.GuildID)
	if err != nil {
		return fmt.Errorf("fetch guild: %w", err)
	}
	serverDir := filepath.Join(r.cfg.OutDir, "server_"+guild.ID)

	var prev *ServerInfo
	if raw, err := os.ReadFile(filepath.Join(serverDir, "server_info.json")); err == nil {
		var s ServerInfo
		if json.Unmarshal(raw, &s) == nil {
			prev = &s
		}
	}
	_ = prev // reserved for future count caching

	humans, mode := r.humanCount(ctx, guild)
	r.log.Info("guild", "name", guild.Name, "humans", humans, "approx_total", guild.ApproximateMembers)

	all, err := r.api.ListChannels(ctx, guild.ID)
	if err != nil {
		return fmt.Errorf("list channels: %w", err)
	}
	cats := map[string]string{}
	for _, ch := range all {
		if ch.Type == discord.ChannelCategory {
			cats[ch.ID] = ch.Name
		}
	}
	channels := r.selectChannels(all)
	if r.cfg.ListChannels {
		r.printChannels(os.Stdout, channels, cats)
		return nil
	}

	var index []ChannelIndex
	var attItems []attach.Item

	// One call per run (v10 API): all active threads, grouped by channel.
	activeByChannel := r.guildActiveThreads(ctx, guild.ID)

	for _, ch := range channels {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, items, err := r.backupChannel(ctx, serverDir, ch, cats, activeByChannel[ch.ID])
		if err != nil {
			r.log.Error("channel failed, continuing", "channel", ch.Name, "err", err)
			continue
		}
		attItems = append(attItems, items...)
		if res == nil {
			continue // skipped (empty / no access)
		}
		index = append(index, *res)
	}

	// Download attachments after all API reads (separate CDN rate limits).
	// Backfills/heals already downloaded their chunks inline (see writeChunk),
	// so an interrupted run keeps both messages and files.
	attOK, attSkip, attFail := r.attOK, r.attSkip, r.attFail
	if r.cfg.DownloadAttachments && len(attItems) > 0 {
		r.log.Info("downloading attachments", "files", len(attItems))
		ok, skip, fail := r.dl.Run(ctx, attItems)
		attOK += ok
		attSkip += skip
		attFail += fail
	}
	if r.cfg.BackfillAttachments {
		r.log.Info("backfilling missing attachments")
		bok, bskip, bfail := r.backfillAttachments(ctx, serverDir)
		attOK += bok
		attSkip += bskip
		attFail += bfail
	}

	info := ServerInfo{
		ID:              guild.ID,
		Name:            guild.Name,
		MemberCount:     humans,
		MemberCountMode: mode,
		ArchivedAt:      time.Now().UTC().Format(time.RFC3339),
		IncludeBots:     r.cfg.IncludeBots,
		Tool:            "discord-backup",
	}
	if guild.Description != nil {
		info.Description = *guild.Description
	}
	if guild.Icon != nil {
		info.Icon = *guild.Icon
	}
	if err := store.WriteJSON(filepath.Join(serverDir, "server_info.json"), info); err != nil {
		return fmt.Errorf("write server_info: %w", err)
	}
	if index == nil {
		index = []ChannelIndex{}
	}
	if err := store.WriteJSON(filepath.Join(serverDir, "channels.json"), index); err != nil {
		return fmt.Errorf("write channels index: %w", err)
	}
	// Permission snapshot (roles + channel overwrites + member roles). Best
	// effort: a failure must not abort the run, the archive is still valid.
	if err := r.writePermissionSnapshot(ctx, guild, all, serverDir); err != nil {
		r.log.Warn("permission snapshot failed, continuing", "err", err)
	}
	r.log.Info("done",
		"channels", len(index),
		"attachments_new", attOK, "attachments_cached", attSkip, "attachments_failed", attFail,
		"server", serverDir)
	return nil
}

// writePermissionSnapshot captures roles, channel overwrites and member
// roles so the archive is self-describing: user-scoped access (MCP) can be
// computed offline, even when Discord is unreachable. Rewritten each run,
// atomically.
func (r *Runner) writePermissionSnapshot(ctx context.Context, guild *discord.Guild, all []discord.Channel, serverDir string) error {
	snap := perms.NewSnapshot(guild.ID)
	snap.SetChannels(all)

	if roles, err := r.api.Roles(ctx, guild.ID); err != nil {
		r.log.Warn("roles unavailable, snapshot without role permissions", "err", err)
	} else {
		snap.Roles = roles
	}

	// Member roles: only enumerated for guilds small enough to page through
	// (same threshold as the member count). Larger guilds rely on a live
	// lookup at query time.
	if guild.ApproximateMembers > 0 && guild.ApproximateMembers <= 10000 {
		after := ""
		for {
			members, err := r.api.ListMembers(ctx, guild.ID, after, 1000)
			if err != nil {
				var fe *discord.ForbiddenError
				if errors.As(err, &fe) {
					r.log.Warn("members forbidden (missing Server Members intent?), snapshot without member roles")
				} else {
					r.log.Warn("members unavailable, snapshot without member roles", "err", err)
				}
				break
			}
			if len(members) == 0 {
				break
			}
			for _, m := range members {
				snap.Members[m.User.ID] = perms.MemberRoles{Username: m.User.Username, Roles: m.Roles}
				after = m.User.ID
			}
			if len(members) < 1000 {
				break
			}
		}
	}

	if err := perms.SaveSnapshot(filepath.Join(serverDir, "permissions.json"), snap); err != nil {
		return err
	}
	r.log.Info("permission snapshot written",
		"roles", len(snap.Roles), "channels", len(snap.Channels), "members", len(snap.Members))
	return nil
}

// selectChannels keeps backup-relevant channels, applies the optional name/ID
// filter and sorts by position.
func (r *Runner) selectChannels(all []discord.Channel) []discord.Channel {
	var out []discord.Channel
	for _, ch := range all {
		switch ch.Type {
		case discord.ChannelGuildText, discord.ChannelVoice,
			discord.ChannelNews, discord.ChannelStageVoice, discord.ChannelForum:
			// backup-relevant
		default:
			continue
		}
		if len(r.cfg.ChannelFilter) > 0 && !matchFilter(ch, r.cfg.ChannelFilter) {
			continue
		}
		out = append(out, ch)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// channelTypeName maps Discord channel types to readable labels.
func channelTypeName(t int) string {
	switch t {
	case discord.ChannelGuildText:
		return "text"
	case discord.ChannelVoice:
		return "voice"
	case discord.ChannelNews:
		return "news"
	case discord.ChannelStageVoice:
		return "stage"
	case discord.ChannelForum:
		return "forum"
	default:
		return fmt.Sprintf("type%d", t)
	}
}

// printChannels renders the channel list as an aligned table, grouped by
// category (positions repeat across categories, so a global sort would mix
// unrelated channels).
func (r *Runner) printChannels(w io.Writer, channels []discord.Channel, cats map[string]string) {
	sorted := append([]discord.Channel(nil), channels...)
	sort.Slice(sorted, func(i, j int) bool {
		ci, cj := categoryOf(sorted[i], cats), categoryOf(sorted[j], cats)
		if ci != cj {
			return ci < cj
		}
		if sorted[i].Position != sorted[j].Position {
			return sorted[i].Position < sorted[j].Position
		}
		return sorted[i].ID < sorted[j].ID
	})
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CHANNEL\tTYPE\tCATEGORY\tID\tTOPIC")
	for _, ch := range sorted {
		topic := ""
		if ch.Topic != nil {
			topic = strings.TrimSpace(*ch.Topic)
			if len(topic) > 60 {
				topic = topic[:57] + "..."
			}
		}
		fmt.Fprintf(tw, "#%s\t%s\t%s\t%s\t%s\n",
			ch.Name, channelTypeName(ch.Type), categoryOf(ch, cats), ch.ID, topic)
	}
	tw.Flush()
	fmt.Fprintf(w, "\n%d channels\n", len(sorted))
}

func categoryOf(ch discord.Channel, cats map[string]string) string {
	if ch.ParentID != nil {
		if c, ok := cats[*ch.ParentID]; ok {
			return c
		}
	}
	return "-"
}

// guildActiveThreads fetches all active threads once per run and groups
// them by parent channel ID. Threads are best-effort data: if the endpoint
// is unavailable, the run continues with messages, pins and archived
// threads rather than failing.
func (r *Runner) guildActiveThreads(ctx context.Context, guildID string) map[string][]discord.Channel {
	out := map[string][]discord.Channel{}
	all, err := r.api.GuildActiveThreads(ctx, guildID)
	if err != nil {
		r.log.Warn("active threads unavailable, continuing without them", "err", err)
		return out
	}
	for _, t := range all {
		if t.ParentID == nil {
			r.log.Warn("active thread without parent, skipping", "thread", t.ID)
			continue
		}
		out[*t.ParentID] = append(out[*t.ParentID], t)
	}
	r.log.Info("active threads", "count", len(all))
	return out
}

func matchFilter(ch discord.Channel, filter []string) bool {
	for _, f := range filter {
		if strings.EqualFold(f, ch.Name) || f == ch.ID {
			return true
		}
	}
	return false
}

// humanCount returns the human (non-bot) member count. Small guilds are
// counted exactly via the members endpoint; large guilds use Discord's
// approximate total to avoid hundreds of paginated requests every run.
func (r *Runner) humanCount(ctx context.Context, guild *discord.Guild) (int, string) {
	if guild.ApproximateMembers <= 0 || guild.ApproximateMembers > 10000 {
		return guild.ApproximateMembers, "approximate"
	}
	count := 0
	after := ""
	for {
		members, err := r.api.ListMembers(ctx, guild.ID, after, 1000)
		if err != nil {
			var fe *discord.ForbiddenError
			if errors.As(err, &fe) {
				r.log.Warn("members endpoint forbidden (missing Server Members intent?), using approximate count")
				return guild.ApproximateMembers, "approximate"
			}
			r.log.Warn("member count failed, using approximate count", "err", err)
			return guild.ApproximateMembers, "approximate"
		}
		if len(members) == 0 {
			break
		}
		for _, m := range members {
			if !m.User.Bot {
				count++
			}
			after = m.User.ID
		}
		if len(members) < 1000 {
			break
		}
	}
	return count, "exact"
}

// --- channels --------------------------------------------------------------

func (r *Runner) backupChannel(ctx context.Context, serverDir string, ch discord.Channel, cats map[string]string, active []discord.Channel) (*ChannelIndex, []attach.Item, error) {
	log := r.log.With("channel", "#"+ch.Name, "id", ch.ID)
	dir := filepath.Join(serverDir, "channel_"+ch.ID)
	cat := ""
	if ch.ParentID != nil {
		cat = cats[*ch.ParentID]
	}
	log.Info("channel start", "type", channelTypeName(ch.Type), "category", cat)

	// Previous counts (or adopt foreign files once by counting lines).
	var prevMsg, prevEdit, prevDel int64
	if raw, err := os.ReadFile(filepath.Join(dir, "channel_info.json")); err == nil {
		var ci ChannelInfo
		if json.Unmarshal(raw, &ci) == nil {
			prevMsg, prevEdit, prevDel = int64(ci.MessageCount), int64(ci.EditCount), int64(ci.DeleteCount)
		}
	} else {
		for _, f := range []struct {
			path string
			dst  *int64
		}{
			{filepath.Join(dir, "messages.jsonl"), &prevMsg},
			{filepath.Join(dir, "edits.jsonl"), &prevEdit},
			{filepath.Join(dir, "deletes.jsonl"), &prevDel},
		} {
			if _, err := os.Stat(f.path); err == nil {
				n, err := store.CountLines(f.path)
				if err != nil {
					return nil, nil, fmt.Errorf("adopt %s: %w", f.path, err)
				}
				*f.dst = n
			}
		}
	}

	var items []attach.Item
	stats := &logStats{}
	noAccess := false

	if ch.Type != discord.ChannelForum {
		s, err := r.backupLog(ctx, dir, ch.ID, "channel #"+ch.Name)
		if err != nil {
			var fe *discord.ForbiddenError
			var ne *discord.NotFoundError
			switch {
			case errors.As(err, &fe), errors.As(err, &ne):
				log.Warn("no access to channel history, keeping existing data", "err", err)
				noAccess = true
			default:
				return nil, nil, err
			}
		} else {
			stats = s
			items = append(items, s.attachments...)
		}
	}

	pinned := 0
	if !noAccess && ch.Type != discord.ChannelForum {
		n, backfill, err := r.backupPins(ctx, dir, ch.ID, log)
		if err != nil {
			var fe *discord.ForbiddenError
			if !errors.As(err, &fe) {
				return nil, nil, fmt.Errorf("pins: %w", err)
			}
			log.Warn("no access to pins")
		} else {
			pinned = n
			items = append(items, backfill...)
		}
	}

	threadCount := 0
	if !noAccess && (ch.Type == discord.ChannelGuildText || ch.Type == discord.ChannelNews || ch.Type == discord.ChannelForum) {
		n, titems, err := r.backupThreads(ctx, dir, ch, active, log)
		if err != nil {
			return nil, nil, err
		}
		threadCount = n
		items = append(items, titems...)
	}

	msgCount := prevMsg + int64(stats.added)
	total := msgCount
	// Thread messages live in their own files; count them for skip-empty.
	if threadCount > 0 {
		tm, err := r.countThreadMessages(dir)
		if err != nil {
			return nil, nil, err
		}
		total += tm
	}

	if r.cfg.SkipEmpty && total == 0 {
		log.Info("skipping empty channel (no access or zero messages)")
		_ = os.RemoveAll(dir)
		return nil, nil, nil
	}

	topic := ""
	if ch.Topic != nil {
		topic = *ch.Topic
	}
	status := "ok"
	if noAccess {
		status = "no_access"
	}
	ci := ChannelInfo{
		ID:           ch.ID,
		Name:         ch.Name,
		Type:         ch.Type,
		Topic:        topic,
		Position:     ch.Position,
		Category:     cat,
		MessageCount: int(msgCount),
		EditCount:    int(prevEdit) + stats.edited,
		DeleteCount:  int(prevDel) + stats.deleted,
		PinnedCount:  pinned,
		ThreadCount:  threadCount,
		BackedUpAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if noAccess {
		ci.NoAccess = true
	}
	if err := store.WriteJSON(filepath.Join(dir, "channel_info.json"), ci); err != nil {
		return nil, nil, err
	}
	log.Info("channel done", "new", stats.added, "edits", stats.edited, "deleted", stats.deleted,
		"threads", threadCount, "pinned", pinned, "status", status)
	return &ChannelIndex{
		ID: ch.ID, Name: ch.Name, Type: ch.Type, Position: ch.Position,
		Category: cat, MessageCount: int(msgCount),
		ThreadCount: threadCount, PinnedCount: pinned, Status: status,
	}, items, nil
}

func (r *Runner) countThreadMessages(channelDir string) (int64, error) {
	root := filepath.Join(channelDir, "threads")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var total int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, e.Name(), "thread_info.json"))
		if err != nil {
			continue
		}
		var ti ThreadInfo
		if json.Unmarshal(raw, &ti) == nil {
			total += int64(ti.MessageCount)
		}
	}
	return total, nil
}

// --- message logs (channels and threads share this) -----------------------

type logStats struct {
	added       int
	edited      int
	deleted     int
	complete    bool // history fetched from the very beginning (no --limit)
	attachments []attach.Item
}

// fetchBeat emits a heartbeat while paging so long histories don't look
// hung: one line every beatEvery, plus a summary if it took >1 page.
// (Suppressed under --quiet like all other info logs.)
type fetchBeat struct {
	log   *slog.Logger
	label string
	phase string
	pages int
	msgs  int
	last  time.Time
}

const beatEvery = 15 * time.Second

func newBeat(log *slog.Logger, label, phase string) *fetchBeat {
	return &fetchBeat{log: log, label: label, phase: phase, last: time.Now()}
}

func (b *fetchBeat) tick(n int) {
	b.pages++
	b.msgs += n
	if time.Since(b.last) >= beatEvery {
		b.log.Info("fetching messages", "log", b.label, "phase", b.phase, "messages", b.msgs, "pages", b.pages)
		b.last = time.Now()
	}
}

func (b *fetchBeat) done() {
	if b.pages > 1 {
		b.log.Info("fetch complete", "log", b.label, "phase", b.phase, "messages", b.msgs, "pages", b.pages)
	}
}

// parseMsgLine adapts envelopes to the store package.
func parseMsgLine(line []byte) (string, time.Time, error) {
	var e discord.Envelope
	if err := json.Unmarshal(line, &e); err != nil {
		return "", time.Time{}, err
	}
	if e.ID == "" || e.Timestamp.IsZero() {
		return "", time.Time{}, fmt.Errorf("bad message line")
	}
	return e.ID, e.Timestamp, nil
}

// backupLog incrementally archives one message log (a channel or a thread)
// into dir/messages.jsonl, with edits in edits.jsonl and delete marks in
// deletes.jsonl.
func (r *Runner) backupLog(ctx context.Context, dir, logID, label string) (*logStats, error) {
	stats := &logStats{}
	msgPath := filepath.Join(dir, "messages.jsonl")
	editPath := filepath.Join(dir, "edits.jsonl")
	delPath := filepath.Join(dir, "deletes.jsonl")
	for _, p := range []string{msgPath, editPath, delPath} {
		if err := store.TruncatePartial(p); err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
	}

	tip, err := store.LastRecord(msgPath, parseMsgLine)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	after := ""
	tipID := ""
	if tip != nil {
		after = tip.ID
		tipID = tip.ID
	}

	// 1) Fetch messages.
	historyMarker := filepath.Join(dir, ".history_complete")
	if tip == nil {
		// Fresh log.
		if r.cfg.Limit > 0 {
			added, lastID, err := r.backfillNewest(ctx, dir, logID, label, r.cfg.Limit, stats)
			if err != nil {
				return nil, err
			}
			stats.added = added
			tipID = lastID
		} else if !r.cfg.Since.IsZero() {
			// Bounded history: page forward from the start date, appending
			// directly. No marker: history is intentionally partial (a later
			// run without --since heals the older part like with --limit).
			if err := r.backfillSince(ctx, dir, logID, label, stats); err != nil {
				return nil, err
			}
			if last, lerr := store.LastRecord(msgPath, parseMsgLine); lerr != nil {
				return nil, fmt.Errorf("%s: %w", label, lerr)
			} else if last != nil {
				tipID = last.ID
			}
		} else {
			if err := r.backfillAll(ctx, dir, logID, label, stats); err != nil {
				return nil, err
			}
			// tipID = newest appended message.
			if last, lerr := store.LastRecord(msgPath, parseMsgLine); lerr != nil {
				return nil, fmt.Errorf("%s: %w", label, lerr)
			} else if last != nil {
				tipID = last.ID
			}
			stats.complete = true
			_ = markHistoryComplete(historyMarker)
		}
	} else {
		fb := newBeat(r.log, label, "catch-up")
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			page, err := r.api.ListMessages(ctx, logID, after, "", 100)
			if err != nil {
				return nil, err
			}
			if len(page) == 0 {
				break
			}
			fb.tick(len(page))
			sortRawAsc(page)
			var batch [][]byte
			for _, raw := range page {
				env, err := discord.ParseEnvelope(raw)
				if err != nil {
					r.log.Warn("skipping unparsable message", "log", label, "err", err)
					continue
				}
				if env.IsBotLike() && !r.cfg.IncludeBots {
					continue
				}
				// The anti-duplicate assertion: storage is ascending, so
				// every appended ID must exceed the previous tip.
				if tipID != "" && !discord.LessID(tipID, env.ID) {
					return nil, fmt.Errorf("%s: API returned non-monotonic id %s after tip %s", label, env.ID, tipID)
				}
				line, err := discord.CompactLine(raw)
				if err != nil {
					return nil, err
				}
				batch = append(batch, line)
				tipID = env.ID
				after = env.ID
				stats.attachments = append(stats.attachments, attachItems(dir, env)...)
			}
			if len(batch) > 0 {
				n, err := store.AppendLines(msgPath, batch)
				if err != nil {
					return nil, fmt.Errorf("%s: append: %w", label, err)
				}
				stats.added += n
			}
			if len(page) < 100 {
				break
			}
		}
		fb.done()
		// One-time self-healing: archives written by versions that only
		// fetched the newest page (100 messages) lack the marker. Fetch
		// everything older than the stored oldest message and prepend it.
		// Users who set --limit/--since asked for partial history: never heal.
		if r.cfg.Limit == 0 && r.cfg.Since.IsZero() {
			if _, merr := os.Stat(historyMarker); os.IsNotExist(merr) {
				healed, herr := r.healHistory(ctx, dir, logID, label, msgPath, stats)
				if herr != nil {
					return nil, herr
				}
				if healed > 0 {
					stats.added += healed
					r.log.Info("healed truncated history", "log", label, "prepended", healed)
				}
				_ = markHistoryComplete(historyMarker)
			}
		}
	}

	// 2) Reconcile the edit window (edits + deletes).
	if r.cfg.EditWindow > 0 {
		e, d, err := r.reconcile(ctx, dir, logID, label, tipID)
		if err != nil {
			return nil, err
		}
		stats.edited = e
		stats.deleted = d
	}
	return stats, nil
}

// backfillNewest stores the newest N messages (ascending) for a fresh log.
func (r *Runner) backfillNewest(ctx context.Context, dir, logID, label string, n int, stats *logStats) (int, string, error) {
	var acc []json.RawMessage // ascending
	before := ""
	for len(acc) < n {
		if err := ctx.Err(); err != nil {
			return 0, "", err
		}
		page, err := r.api.ListMessages(ctx, logID, "", before, 100)
		if err != nil {
			return 0, "", err
		}
		if len(page) == 0 {
			break
		}
		sortRawAsc(page)
		acc = append(page, acc...)
		before = mustID(page[0])
		if len(page) < 100 {
			break
		}
	}
	if len(acc) > n {
		acc = acc[len(acc)-n:]
	}
	var batch [][]byte
	lastID := ""
	for _, raw := range acc {
		env, err := discord.ParseEnvelope(raw)
		if err != nil {
			continue
		}
		if env.IsBotLike() && !r.cfg.IncludeBots {
			continue
		}
		line, err := discord.CompactLine(raw)
		if err != nil {
			return 0, "", err
		}
		batch = append(batch, line)
		lastID = env.ID
		stats.attachments = append(stats.attachments, attachItems(dir, env)...)
	}
	if len(batch) > 0 {
		m, err := store.AppendLines(filepath.Join(dir, "messages.jsonl"), batch)
		if err != nil {
			return 0, "", err
		}
		_ = m
	}
	return len(batch), lastID, nil
}

// backfillSince fetches a fresh log forward from the --since date, appending
// directly (no staging needed: `after` pages oldest-window-first). The start
// date becomes a snowflake cursor, so no request is wasted on older history.
func (r *Runner) backfillSince(ctx context.Context, dir, logID, label string, stats *logStats) error {
	msgPath := filepath.Join(dir, "messages.jsonl")
	after := discord.TimeSnowflake(r.cfg.Since)
	tipID := ""
	fb := newBeat(r.log, label, "since")
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := r.api.ListMessages(ctx, logID, after, "", 100)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if len(page) == 0 {
			break
		}
		fb.tick(len(page))
		sortRawAsc(page)
		var batch [][]byte
		for _, raw := range page {
			env, err := discord.ParseEnvelope(raw)
			if err != nil {
				r.log.Warn("skipping unparsable message", "log", label, "err", err)
				continue
			}
			if env.IsBotLike() && !r.cfg.IncludeBots {
				continue
			}
			if tipID != "" && !discord.LessID(tipID, env.ID) {
				return fmt.Errorf("%s: API returned non-monotonic id %s after tip %s", label, env.ID, tipID)
			}
			line, err := discord.CompactLine(raw)
			if err != nil {
				return err
			}
			batch = append(batch, line)
			tipID = env.ID
			after = env.ID
			stats.attachments = append(stats.attachments, attachItems(dir, env)...)
		}
		if len(batch) > 0 {
			n, err := store.AppendLines(msgPath, batch)
			if err != nil {
				return fmt.Errorf("%s: append: %w", label, err)
			}
			stats.added += n
		}
		if len(page) < 100 {
			break
		}
	}
	fb.done()
	return nil
}

// backfillAll fetches the complete history of a fresh log (oldest to
// newest). Discord only pages newest-first (max 100 per call), so collect
// pages backwards via `before` and flush oldest-first in chunks: every chunk
// is durable on disk, so an interrupted run resumes instead of restarting
// (the next run catches up newer messages and heals older ones).
// Memory stays bounded by flushEveryPages; only the cursor advances in RAM.
const flushEveryPages = 50 // 50 pages x 100 messages per flush

func (r *Runner) backfillAll(ctx context.Context, dir, logID, label string, stats *logStats) error {
	msgPath := filepath.Join(dir, "messages.jsonl")
	var buf [][]json.RawMessage
	before := ""
	appendMode := true
	if fi, err := os.Stat(msgPath); err == nil && fi.Size() > 0 {
		appendMode = false // defensive: never append onto unknown content
	}
	fb := newBeat(r.log, label, "backfill")
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		n, err := r.writeChunk(ctx, dir, msgPath, label, buf, appendMode, stats)
		stats.added += n
		if err != nil {
			return err
		}
		appendMode = false
		buf = nil
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := r.api.ListMessages(ctx, logID, "", before, 100)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if len(page) == 0 {
			break
		}
		fb.tick(len(page))
		sortRawAsc(page)
		oldest := mustID(page[0])
		buf = append(buf, page)
		if len(buf) >= flushEveryPages {
			if err := flush(); err != nil {
				return err
			}
		}
		if oldest == "" || oldest == before {
			break // defensive: cursor did not advance
		}
		before = oldest
		if len(page) < 100 {
			break
		}
	}
	fb.done()
	return flush()
}

// healHistory prepends messages older than the stored oldest record.
// One-time repair for archives truncated to the newest 100 messages by
// older versions. Flushes in chunks like backfillAll, so an interrupted
// heal resumes on the next run (the marker is only written on success).
// Returns the number of prepended messages.
func (r *Runner) healHistory(ctx context.Context, dir, logID, label, msgPath string, stats *logStats) (int, error) {
	oldest, err := store.FirstRecord(msgPath, parseMsgLine)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", label, err)
	}
	if oldest == nil {
		return 0, nil
	}
	var buf [][]json.RawMessage
	before := oldest.ID
	fb := newBeat(r.log, label, "heal")
	healed := 0
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		// History exists: older chunks always go in front.
		n, err := r.writeChunk(ctx, dir, msgPath, label, buf, false, stats)
		healed += n
		if err != nil {
			return err
		}
		buf = nil
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return healed, err
		}
		page, err := r.api.ListMessages(ctx, logID, "", before, 100)
		if err != nil {
			return healed, fmt.Errorf("%s: %w", label, err)
		}
		if len(page) == 0 {
			break
		}
		fb.tick(len(page))
		sortRawAsc(page)
		next := mustID(page[0])
		buf = append(buf, page)
		if len(buf) >= flushEveryPages {
			if err := flush(); err != nil {
				return healed, err
			}
		}
		if next == "" || next == before {
			break
		}
		before = next
		if len(page) < 100 {
			break
		}
	}
	fb.done()
	if err := flush(); err != nil {
		return healed, err
	}
	return healed, nil
}

// writeChunk writes buffered pages (newest-first) to msgPath in ascending
// order and returns the lines written. appendMode appends (fresh log),
// otherwise the chunk is prepended (older history). Attachments download
// inline when enabled, so an interrupted run keeps both messages and files;
// otherwise items are queued for the end-of-run batch.
func (r *Runner) writeChunk(ctx context.Context, dir, msgPath, label string, buf [][]json.RawMessage, appendMode bool, stats *logStats) (int, error) {
	var batch [][]byte
	var items []attach.Item
	for i := len(buf) - 1; i >= 0; i-- {
		for _, raw := range buf[i] {
			env, err := discord.ParseEnvelope(raw)
			if err != nil {
				r.log.Warn("skipping unparsable message", "log", label, "err", err)
				continue
			}
			if env.IsBotLike() && !r.cfg.IncludeBots {
				continue
			}
			line, err := discord.CompactLine(raw)
			if err != nil {
				return 0, err
			}
			batch = append(batch, line)
			items = append(items, attachItems(dir, env)...)
		}
	}
	if len(batch) == 0 {
		return 0, nil
	}
	var n int
	var err error
	if appendMode {
		n, err = store.AppendLines(msgPath, batch)
	} else {
		n, err = store.PrependLines(msgPath, batch)
	}
	if err != nil {
		return n, fmt.Errorf("%s: write chunk: %w", label, err)
	}
	if r.cfg.DownloadAttachments && len(items) > 0 {
		r.log.Debug("downloading attachments", "log", label, "files", len(items))
		ok, skip, fail := r.dl.Run(ctx, items)
		r.attOK += ok
		r.attSkip += skip
		r.attFail += fail
	} else {
		stats.attachments = append(stats.attachments, items...)
	}
	return n, nil
}

func markHistoryComplete(marker string) error {
	if err := store.EnsureDir(filepath.Dir(marker)); err != nil {
		return err
	}
	return os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

func mustID(raw json.RawMessage) string {
	env, err := discord.ParseEnvelope(raw)
	if err != nil {
		return ""
	}
	return env.ID
}

func sortRawAsc(page []json.RawMessage) {
	sort.Slice(page, func(i, j int) bool {
		return discord.LessID(mustID(page[i]), mustID(page[j]))
	})
}

// reconcile re-fetches the recent window and records edits (new full
// versions in edits.jsonl) and deletions (marks in deletes.jsonl).
// messages.jsonl itself is never modified, so history stays append-only.
func (r *Runner) reconcile(ctx context.Context, dir, logID, label, tipID string) (edited, deleted int, err error) {
	msgPath := filepath.Join(dir, "messages.jsonl")
	editPath := filepath.Join(dir, "edits.jsonl")
	delPath := filepath.Join(dir, "deletes.jsonl")
	since := time.Now().UTC().Add(-r.cfg.EditWindow)
	sinceID := discord.TimeSnowflake(since)

	// Remote window (ascending, bot-filtered like local storage).
	var remote []remoteMsg
	after := sinceID
	truncated := false
outer:
	for {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		page, err := r.api.ListMessages(ctx, logID, after, "", 100)
		if err != nil {
			return 0, 0, err
		}
		if len(page) == 0 {
			break
		}
		sortRawAsc(page)
		for _, raw := range page {
			env, err := discord.ParseEnvelope(raw)
			if err != nil {
				continue
			}
			if env.IsBotLike() && !r.cfg.IncludeBots {
				continue
			}
			line, err := discord.CompactLine(raw)
			if err != nil {
				return 0, 0, err
			}
			remote = append(remote, remoteMsg{env: env, raw: line})
			after = env.ID
			if len(remote) >= reconcileCap {
				truncated = true
				break outer
			}
		}
		if len(page) < 100 {
			break
		}
	}

	// Local window state = messages overlaid with already-known edits
	// (newest edit wins), so repeated runs do not duplicate edit entries.
	scanSince := since.Add(-5 * time.Minute)
	local, err := store.Recent(msgPath, scanSince, recentScanCap, parseMsgLine)
	if err != nil {
		return 0, 0, err
	}
	state := make(map[string]json.RawMessage, len(local))
	for _, rec := range local {
		state[rec.ID] = rec.Raw
	}
	edits, err := store.Recent(editPath, scanSince, recentScanCap, parseMsgLine)
	if err != nil {
		return 0, 0, err
	}
	for _, rec := range edits {
		state[rec.ID] = rec.Raw
	}
	knownDeletes, err := store.AllIDs(delPath, parseDeleteLine)
	if err != nil {
		return 0, 0, err
	}

	var editBatch [][]byte
	for _, rm := range remote {
		cur, ok := state[rm.env.ID]
		switch {
		case !ok:
			if tipID != "" && discord.LessID(tipID, rm.env.ID) {
				// Newer than anything stored: missed catch-up (e.g. a
				// previous run died between fetch and append). Append in
				// order — remote is ascending and all such IDs are new.
				n, err := store.AppendLines(msgPath, [][]byte{rm.raw})
				if err != nil {
					return edited, deleted, err
				}
				if n == 1 {
					tipID = rm.env.ID
				}
			}
			// Older than local history (e.g. --limit backfill): ignore.
		default:
			same, err := discord.SameContent(cur, rm.raw)
			if err != nil {
				continue
			}
			if !same {
				editBatch = append(editBatch, rm.raw)
			}
		}
	}
	// Batch the edit appends (one fsync instead of one per message).
	if len(editBatch) > 0 {
		// Re-check each against the freshest state is unnecessary: state was
		// just built from disk and this process is the only writer (lock).
		n, err := store.AppendLines(editPath, editBatch)
		if err != nil {
			return edited, deleted, err
		}
		edited = n
		// Download attachments of edited messages (new IDs only; the
		// downloader skips files that already exist).
		_ = n
	}

	// Delete detection only on a complete window; a truncated rescan cannot
	// prove absence.
	if !truncated {
		remoteSet := make(map[string]struct{}, len(remote))
		minRemote := ""
		for _, rm := range remote {
			remoteSet[rm.env.ID] = struct{}{}
			if minRemote == "" || discord.LessID(rm.env.ID, minRemote) {
				minRemote = rm.env.ID
			}
		}
		var delBatch [][]byte
		now := time.Now().UTC().Format(time.RFC3339)
		for _, rec := range local {
			if rec.TS.Before(since) {
				continue
			}
			if minRemote != "" && discord.LessID(rec.ID, minRemote) {
				continue
			}
			if _, ok := remoteSet[rec.ID]; ok {
				continue
			}
			if _, ok := knownDeletes[rec.ID]; ok {
				continue
			}
			mark, _ := json.Marshal(DeleteMark{ID: rec.ID, DetectedAt: now})
			delBatch = append(delBatch, mark)
		}
		if len(delBatch) > 0 {
			n, err := store.AppendLines(delPath, delBatch)
			if err != nil {
				return edited, deleted, err
			}
			deleted = n
		}
	} else {
		r.log.Warn("edit window truncated, skipping delete detection for this run", "log", label, "cap", reconcileCap)
	}
	return edited, deleted, nil
}

type remoteMsg struct {
	env *discord.Envelope
	raw []byte
}

func parseDeleteLine(line []byte) (string, time.Time, error) {
	var m DeleteMark
	if err := json.Unmarshal(line, &m); err != nil {
		return "", time.Time{}, err
	}
	if m.ID == "" {
		return "", time.Time{}, fmt.Errorf("bad delete line")
	}
	return m.ID, time.Now(), nil
}

func attachItems(dir string, env *discord.Envelope) []attach.Item {
	var out []attach.Item
	for _, a := range env.Attachments {
		if a.URL == "" {
			continue
		}
		out = append(out, attach.Item{
			URL:          a.URL,
			MessageID:    env.ID,
			AttachmentID: a.ID,
			Filename:     a.Filename,
			DestDir:      filepath.Join(dir, "attachments"),
		})
	}
	return out
}

// backfillAttachments scans every stored log (channels and threads) for
// attachments missing on disk – e.g. archived before --download-attachments
// was enabled, or skipped by an older size cap – and downloads them.
// Message data is never modified. Returns (new, cached, failed).
func (r *Runner) backfillAttachments(ctx context.Context, serverDir string) (ok, skipped, failed int) {
	var missing []attach.Item
	var files, msgs int
	scan := func(dir, what string) {
		for _, name := range []string{"messages.jsonl", "pins_backfill.jsonl", "edits.jsonl"} {
			items, n, err := missingInLog(filepath.Join(dir, name), dir)
			if err != nil {
				r.log.Warn("backfill scan failed, continuing", "log", what, "file", name, "err", err)
				continue
			}
			files++
			msgs += n
			missing = append(missing, items...)
		}
	}
	entries, err := os.ReadDir(serverDir)
	if err != nil {
		r.log.Warn("backfill scan failed", "err", err)
		return 0, 0, 0
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "channel_") {
			continue
		}
		cdir := filepath.Join(serverDir, e.Name())
		scan(cdir, e.Name())
		tentries, err := os.ReadDir(filepath.Join(cdir, "threads"))
		if err != nil {
			continue // no threads, or unreadable: not fatal
		}
		for _, t := range tentries {
			if !t.IsDir() {
				continue
			}
			scan(filepath.Join(cdir, "threads", t.Name()), t.Name())
		}
	}
	r.log.Info("backfill scan complete", "files", files, "messages", msgs, "missing", len(missing))
	if len(missing) == 0 {
		return 0, 0, 0
	}
	if err := ctx.Err(); err != nil {
		r.log.Warn("backfill download skipped, interrupted")
		return 0, 0, 0
	}
	return r.dl.Run(ctx, missing)
}

// missingInLog returns download items for attachments referenced in a stored
// log file but absent on disk, plus the number of messages scanned.
func missingInLog(path, dir string) ([]attach.Item, int, error) {
	var out []attach.Item
	msgs := 0
	err := store.ForEachLine(path, func(line []byte) error {
		env, err := discord.ParseEnvelope(line)
		if err != nil {
			return nil // skip unreadable lines, never abort the scan
		}
		msgs++
		for _, it := range attachItems(dir, env) {
			dest := filepath.Join(it.DestDir, attach.DestName(it.MessageID, it.AttachmentID, it.Filename))
			if _, err := os.Stat(dest); err == nil {
				continue
			} else if !os.IsNotExist(err) {
				return err
			}
			out = append(out, it)
		}
		return nil
	})
	return out, msgs, err
}

// --- pins ------------------------------------------------------------------

func (r *Runner) backupPins(ctx context.Context, dir, channelID string, log *slog.Logger) (count int, backfill []attach.Item, err error) {
	pins, err := r.api.ListPins(ctx, channelID)
	if err != nil {
		return 0, nil, err
	}
	var ids []string
	var raws []json.RawMessage
	for _, raw := range pins {
		env, err := discord.ParseEnvelope(raw)
		if err != nil {
			continue
		}
		if env.IsBotLike() && !r.cfg.IncludeBots {
			continue
		}
		ids = append(ids, env.ID)
		raws = append(raws, raw)
	}
	sort.Strings(ids)

	// Skip all disk work when the pin set is unchanged since last run.
	prev := PinsSnapshot{}
	if raw, rerr := os.ReadFile(filepath.Join(dir, "pins.json")); rerr == nil {
		_ = json.Unmarshal(raw, &prev)
	}
	if equalStrings(prev.MessageIDs, ids) && len(ids) > 0 {
		return len(ids), nil, nil
	}

	// Backfill pinned messages that predate the archive into a separate
	// file, so messages.jsonl keeps its ascending order invariant.
	if len(ids) > 0 {
		known, err := store.AllIDs(filepath.Join(dir, "messages.jsonl"), parseMsgLine)
		if err != nil {
			return 0, nil, err
		}
		var lines [][]byte
		for _, raw := range raws {
			env, _ := discord.ParseEnvelope(raw) // already validated
			if _, ok := known[env.ID]; ok {
				continue
			}
			line, err := discord.CompactLine(raw)
			if err != nil {
				return 0, nil, err
			}
			lines = append(lines, line)
			backfill = append(backfill, attachItems(dir, env)...)
		}
		if len(lines) > 0 {
			if _, err := store.AppendLines(filepath.Join(dir, "pins_backfill.jsonl"), lines); err != nil {
				return 0, nil, err
			}
			log.Info("backfilled old pinned messages", "count", len(lines))
		}
	}
	snap := PinsSnapshot{FetchedAt: time.Now().UTC().Format(time.RFC3339), MessageIDs: ids}
	if snap.MessageIDs == nil {
		snap.MessageIDs = []string{}
	}
	if err := store.WriteJSON(filepath.Join(dir, "pins.json"), snap); err != nil {
		return 0, nil, err
	}
	return len(ids), backfill, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

// --- threads ---------------------------------------------------------------

func (r *Runner) backupThreads(ctx context.Context, dir string, ch discord.Channel, active []discord.Channel, log *slog.Logger) (count int, items []attach.Item, err error) {
	seen := map[string]discord.Channel{}
	add := func(list []discord.Channel) {
		for _, t := range list {
			if _, ok := seen[t.ID]; !ok {
				seen[t.ID] = t
			}
		}
	}
	add(active)
	for _, kind := range []string{"public", "private", "joined"} {
		before := ""
		for {
			page, more, err := r.api.ArchivedThreads(ctx, ch.ID, kind, before, 100)
			if err != nil {
				var fe *discord.ForbiddenError
				var ne *discord.NotFoundError
				if errors.As(err, &fe) || errors.As(err, &ne) {
					log.Warn("no access to archived threads", "kind", kind, "err", err)
					break
				}
				return 0, nil, fmt.Errorf("archived threads (%s): %w", kind, err)
			}
			add(page)
			if !more || len(page) == 0 {
				break
			}
			// Cursor = archive timestamp of the oldest thread seen.
			oldest := page[len(page)-1]
			if oldest.ThreadMeta == nil || oldest.ThreadMeta.ArchiveTimestamp == "" {
				break
			}
			before = oldest.ThreadMeta.ArchiveTimestamp
		}
	}

	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		t := seen[id]
		tdir := filepath.Join(dir, "threads", "thread_"+t.ID)
		archived, locked := true, false
		if t.ThreadMeta != nil {
			archived = t.ThreadMeta.Archived
			locked = t.ThreadMeta.Locked
		}
		// Active threads are, by definition, not archived.
		for _, a := range active {
			if a.ID == t.ID {
				archived = false
			}
		}
		// Previous counts BEFORE backupLog runs (backupLog appends, so
		// anything counted afterwards would double-count).
		var pm, pe, pd int64
		if raw, rerr := os.ReadFile(filepath.Join(tdir, "thread_info.json")); rerr == nil {
			var prev ThreadInfo
			if json.Unmarshal(raw, &prev) == nil {
				pm, pe, pd = int64(prev.MessageCount), int64(prev.EditCount), int64(prev.DeleteCount)
			}
		} else if _, serr := os.Stat(filepath.Join(tdir, "messages.jsonl")); serr == nil {
			if n, cerr := store.CountLines(filepath.Join(tdir, "messages.jsonl")); cerr == nil {
				pm = n
			}
		}
		stats, terr := r.backupLog(ctx, tdir, t.ID, "thread "+t.Name)
		tNoAccess := false
		if terr != nil {
			var fe *discord.ForbiddenError
			var ne *discord.NotFoundError
			if errors.As(terr, &fe) || errors.As(terr, &ne) {
				tNoAccess = true
				stats = &logStats{}
				log.Warn("no access to thread history", "thread", t.Name)
			} else {
				return 0, nil, fmt.Errorf("thread %s: %w", t.Name, terr)
			}
		}
		items = append(items, stats.attachments...)
		owner := ""
		if t.OwnerID != nil {
			owner = *t.OwnerID
		}
		parent := ch.ID
		if t.ParentID != nil {
			parent = *t.ParentID
		}
		ti := ThreadInfo{
			ID: t.ID, Name: t.Name, ParentID: parent, OwnerID: owner,
			Type: t.Type, Archived: archived, Locked: locked,
			MessageCount: int(pm) + stats.added,
			EditCount:    int(pe) + stats.edited,
			DeleteCount:  int(pd) + stats.deleted,
			BackedUpAt:   time.Now().UTC().Format(time.RFC3339),
		}
		if tNoAccess {
			ti.NoAccess = true
		}
		if err := store.WriteJSON(filepath.Join(tdir, "thread_info.json"), ti); err != nil {
			return 0, nil, err
		}
		count++
	}
	return count, items, nil
}
