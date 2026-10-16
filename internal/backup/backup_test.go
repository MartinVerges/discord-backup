package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"discord-backup/internal/discord"
	"discord-backup/internal/store"
)

// fakeDiscord is a minimal in-memory Discord API for backup tests.
type fakeDiscord struct {
	msgs    []*fakeMsg            // channel C, ascending by id
	threads []fakeThread          // active guild threads
	tmsgs   map[string][]*fakeMsg // thread messages by thread id
}

type fakeThread struct {
	id     string
	name   string
	parent string
}

type fakeMsg struct {
	id      string
	content string
	ts      time.Time
	edited  *time.Time
	bot     bool
}

func (f *fakeDiscord) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	p := r.URL.Path
	switch {
	case p == "/users/@me":
		write(map[string]any{"id": "BOT", "username": "archiver", "bot": true})
	case strings.HasPrefix(p, "/guilds/G/members"):
		write([]any{})
	case p == "/guilds/G":
		write(map[string]any{"id": "G", "name": "Test Guild", "approximate_member_count": 0})
	case p == "/guilds/G/channels":
		write([]any{map[string]any{"id": "C", "name": "general", "type": 0, "position": 0}})
	case p == "/channels/C/pins":
		write([]any{})
	case p == "/guilds/G/threads/active":
		var out []any
		for _, t := range f.threads {
			out = append(out, map[string]any{
				"id": t.id, "name": t.name, "type": 11, "parent_id": t.parent,
				"owner_id":        "U1",
				"thread_metadata": map[string]any{"archived": false, "archive_timestamp": time.Now().UTC().Format(time.RFC3339), "locked": false},
			})
		}
		if out == nil {
			out = []any{}
		}
		write(map[string]any{"threads": out, "members": []any{}})
	case strings.Contains(p, "/threads/archived/"):
		write(map[string]any{"threads": []any{}, "has_more": false})
	case strings.HasPrefix(p, "/channels/") && strings.HasSuffix(p, "/messages"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/channels/"), "/messages")
		msgs := f.msgs
		if id != "C" {
			msgs = f.tmsgs[id]
		}
		q := r.URL.Query()
		after, before := q.Get("after"), q.Get("before")
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit < 1 {
			limit = 100
		}
		var out []any
		// API returns newest first.
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			if after != "" && !discord.LessID(after, m.id) {
				continue
			}
			if before != "" && !discord.LessID(m.id, before) {
				continue
			}
			out = append(out, f.msgJSON(id, m))
			if len(out) >= limit {
				break
			}
		}
		if out == nil {
			out = []any{}
		}
		write(out)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeDiscord) msgJSON(channelID string, m *fakeMsg) map[string]any {
	v := map[string]any{
		"id": m.id, "channel_id": channelID, "content": m.content,
		"timestamp":   m.ts.UTC().Format("2006-01-02T15:04:05.000000+00:00"),
		"author":      map[string]any{"id": "U1", "username": "alice", "bot": m.bot},
		"attachments": []any{}, "embeds": []any{},
	}
	if m.edited != nil {
		v["edited_timestamp"] = m.edited.UTC().Format("2006-01-02T15:04:05.000000+00:00")
	}
	return v
}

func msgID(t time.Time) string { return discord.TimeSnowflake(t) }

func testCfg(t *testing.T, base, out string) Config {
	t.Helper()
	return Config{
		Token: "x", GuildID: "G", APIBase: base, OutDir: out,
		EditWindow: 3 * time.Hour, SkipEmpty: true,
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	n, err := store.CountLines(path)
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

func channelDir(out string) string { return filepath.Join(out, "server_G", "channel_C") }

func TestIncrementalNoDuplicates(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	fake := &fakeDiscord{msgs: []*fakeMsg{
		{id: msgID(now.Add(-2 * time.Hour)), content: "one", ts: now.Add(-2 * time.Hour)},
		{id: msgID(now.Add(-90 * time.Minute)), content: "two", ts: now.Add(-90 * time.Minute)},
		{id: msgID(now.Add(-time.Hour)), content: "three", ts: now.Add(-time.Hour)},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	out := t.TempDir()
	ctx := context.Background()

	run := func() {
		t.Helper()
		if err := Run(ctx, testCfg(t, srv.URL, out)); err != nil {
			t.Fatal(err)
		}
	}
	msgPath := filepath.Join(channelDir(out), "messages.jsonl")
	editPath := filepath.Join(channelDir(out), "edits.jsonl")
	delPath := filepath.Join(channelDir(out), "deletes.jsonl")

	// 1) First run archives everything.
	run()
	if n := countLines(t, msgPath); n != 3 {
		t.Fatalf("first run: %d lines", n)
	}

	// 2) Second run must not duplicate anything (the original project's bug
	// was last_message_id=None triggering a full re-append).
	run()
	if n := countLines(t, msgPath); n != 3 {
		t.Fatalf("second run duplicated: %d lines", n)
	}

	// 3) Edit message two -> exactly one edit entry, no message dup.
	editedAt := now.Add(-30 * time.Minute)
	fake.msgs[1].content = "two (edited)"
	fake.msgs[1].edited = &editedAt
	run()
	if n := countLines(t, msgPath); n != 3 {
		t.Fatalf("edit run duplicated messages: %d", n)
	}
	if n := countLines(t, editPath); n != 1 {
		t.Fatalf("edit run: %d edit lines, want 1", n)
	}
	raw, err := os.ReadFile(editPath)
	if err != nil || !strings.Contains(string(raw), "two (edited)") {
		t.Fatalf("edit content missing: %v %q", err, raw)
	}

	// 4) Re-run: edit must not be recorded twice.
	run()
	if n := countLines(t, editPath); n != 1 {
		t.Fatalf("edit duplicated: %d lines", n)
	}

	// 5) Delete message three -> one delete mark, history untouched.
	deletedID := fake.msgs[2].id
	fake.msgs = fake.msgs[:2]
	run()
	if n := countLines(t, msgPath); n != 3 {
		t.Fatalf("delete run changed history: %d", n)
	}
	if n := countLines(t, delPath); n != 1 {
		t.Fatalf("delete run: %d delete lines, want 1", n)
	}
	// The delete mark must reference message three's id.
	lines, err := os.ReadFile(delPath)
	if err != nil {
		t.Fatal(err)
	}
	var mark DeleteMark
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(lines))), &mark); err != nil {
		t.Fatal(err)
	}
	if mark.ID != deletedID {
		t.Fatalf("delete mark id %q, want %q", mark.ID, deletedID)
	}
	// 6) New message arrives -> appended once, in order.
	m4 := &fakeMsg{id: msgID(now.Add(-10 * time.Minute)), content: "four", ts: now.Add(-10 * time.Minute)}
	fake.msgs = append(fake.msgs, m4)
	run()
	if n := countLines(t, msgPath); n != 4 {
		t.Fatalf("new message run: %d lines", n)
	}
	last, err := store.LastLine(msgPath)
	if err != nil || !strings.Contains(string(last), `"four"`) {
		t.Fatalf("last line is not the new message: %v %q", err, last)
	}
	run()
	if n := countLines(t, msgPath); n != 4 {
		t.Fatalf("stable rerun: %d lines", n)
	}
}

func TestBotFiltering(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	fake := &fakeDiscord{msgs: []*fakeMsg{
		{id: msgID(now.Add(-time.Hour)), content: "human", ts: now.Add(-time.Hour)},
		{id: msgID(now.Add(-30 * time.Minute)), content: "beep", ts: now.Add(-30 * time.Minute), bot: true},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	out := t.TempDir()

	cfg := testCfg(t, srv.URL, out)
	if err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if n := countLines(t, filepath.Join(channelDir(out), "messages.jsonl")); n != 1 {
		t.Fatalf("bot message archived despite filter: %d", n)
	}

	cfg.IncludeBots = true
	if err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	// Incremental: only the previously skipped bot message is appended.
	if n := countLines(t, filepath.Join(channelDir(out), "messages.jsonl")); n != 2 {
		t.Fatalf("include-bots run: %d lines, want 2", n)
	}
}

func TestLimitBackfill(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var msgs []*fakeMsg
	for i := 0; i < 5; i++ {
		ts := now.Add(-time.Duration(5-i) * time.Hour)
		msgs = append(msgs, &fakeMsg{id: msgID(ts), content: fmt.Sprintf("m%d", i), ts: ts})
	}
	fake := &fakeDiscord{msgs: msgs}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	out := t.TempDir()

	cfg := testCfg(t, srv.URL, out)
	cfg.Limit = 2
	// Window only covers recent history; older messages are out of scope.
	cfg.EditWindow = 30 * time.Minute
	if err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(channelDir(out), "messages.jsonl")
	if n := countLines(t, msgPath); n != 2 {
		t.Fatalf("limited backfill: %d lines, want 2", n)
	}
	// Ascending order, newest two.
	recs, err := store.Recent(msgPath, now.Add(-3*time.Hour), 10, parseMsgLine)
	if err != nil {
		t.Fatal(err)
	}
	var contents []string
	for _, r := range recs {
		var e discord.Envelope
		_ = json.Unmarshal(r.Raw, &e)
		contents = append(contents, e.Content)
	}
	sort.Strings(contents)
	if len(contents) != 2 || contents[0] != "m3" || contents[1] != "m4" {
		t.Fatalf("backfill contents: %v", contents)
	}
	// Second run: stable, and the out-of-scope older messages must NOT be
	// appended as "missing" (order invariant would break).
	if err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if n := countLines(t, msgPath); n != 2 {
		t.Fatalf("limited rerun: %d lines", n)
	}
}

func TestFullHistoryBeyondOnePage(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var msgs []*fakeMsg
	for i := 0; i < 250; i++ {
		ts := now.Add(-time.Duration(250-i) * time.Minute)
		msgs = append(msgs, &fakeMsg{id: msgID(ts), content: fmt.Sprintf("m%03d", i), ts: ts})
	}
	fake := &fakeDiscord{msgs: msgs}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	out := t.TempDir()

	cfg := testCfg(t, srv.URL, out)
	if err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(channelDir(out), "messages.jsonl")
	if n := countLines(t, msgPath); n != 250 {
		t.Fatalf("full history: %d lines, want 250 (must page beyond Discord's 100/page)", n)
	}
	// Ascending order preserved across pages.
	recs, err := store.Recent(msgPath, now.Add(-300*time.Minute), 300, parseMsgLine)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 250 {
		t.Fatalf("recent: %d records, want 250", len(recs))
	}
	for i := 1; i < len(recs); i++ {
		if !discord.LessID(recs[i-1].ID, recs[i].ID) {
			t.Fatalf("order broken at %d: %s >= %s", i, recs[i-1].ID, recs[i].ID)
		}
	}
	// Rerun: stable, no duplicates.
	if err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if n := countLines(t, msgPath); n != 250 {
		t.Fatalf("rerun: %d lines, want 250", n)
	}
}

func TestHealTruncatedHistory(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var msgs []*fakeMsg
	for i := 0; i < 250; i++ {
		ts := now.Add(-time.Duration(250-i) * time.Minute)
		msgs = append(msgs, &fakeMsg{id: msgID(ts), content: fmt.Sprintf("m%03d", i), ts: ts})
	}
	fake := &fakeDiscord{msgs: msgs}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	out := t.TempDir()
	ctx := context.Background()

	// Simulate an archive written by the buggy version: only newest 100.
	cfg := testCfg(t, srv.URL, out)
	cfg.Limit = 100
	if err := Run(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(channelDir(out), "messages.jsonl")
	if n := countLines(t, msgPath); n != 100 {
		t.Fatalf("setup: %d lines, want 100", n)
	}
	// Next run without limit must heal: prepend the missing 150.
	cfg.Limit = 0
	if err := Run(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if n := countLines(t, msgPath); n != 250 {
		t.Fatalf("healed: %d lines, want 250", n)
	}
	recs, err := store.Recent(msgPath, now.Add(-300*time.Minute), 300, parseMsgLine)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(recs); i++ {
		if !discord.LessID(recs[i-1].ID, recs[i].ID) {
			t.Fatalf("order broken at %d", i)
		}
	}
	// Third run: stable.
	if err := Run(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if n := countLines(t, msgPath); n != 250 {
		t.Fatalf("stable: %d lines, want 250", n)
	}
}

func TestThreadBackup(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	fake := &fakeDiscord{
		msgs: []*fakeMsg{
			{id: msgID(now.Add(-time.Hour)), content: "parent msg", ts: now.Add(-time.Hour)},
		},
		threads: []fakeThread{{id: "T1", name: "a thread", parent: "C"}},
		tmsgs: map[string][]*fakeMsg{
			"T1": {{id: msgID(now.Add(-30 * time.Minute)), content: "thread reply", ts: now.Add(-30 * time.Minute)}},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	out := t.TempDir()
	ctx := context.Background()

	if err := Run(ctx, testCfg(t, srv.URL, out)); err != nil {
		t.Fatal(err)
	}
	tdir := filepath.Join(channelDir(out), "threads", "thread_T1")
	if n := countLines(t, filepath.Join(tdir, "messages.jsonl")); n != 1 {
		t.Fatalf("thread messages: %d lines, want 1", n)
	}
	raw, err := os.ReadFile(filepath.Join(tdir, "thread_info.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ti ThreadInfo
	if err := json.Unmarshal(raw, &ti); err != nil {
		t.Fatal(err)
	}
	if ti.MessageCount != 1 || ti.Archived {
		t.Fatalf("thread info: %+v", ti)
	}
	// Rerun: stable, no duplicates in thread log.
	if err := Run(ctx, testCfg(t, srv.URL, out)); err != nil {
		t.Fatal(err)
	}
	if n := countLines(t, filepath.Join(tdir, "messages.jsonl")); n != 1 {
		t.Fatalf("thread rerun duplicated: %d lines", n)
	}
}

func TestSkipEmptyNoAccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/@me" {
			_, _ = w.Write([]byte(`{"id":"B","username":"b","bot":true}`))
			return
		}
		if r.URL.Path == "/guilds/G" {
			_, _ = w.Write([]byte(`{"id":"G","name":"g","approximate_member_count":1}`))
			return
		}
		if r.URL.Path == "/guilds/G/channels" {
			_, _ = w.Write([]byte(`[{"id":"C","name":"secret","type":0,"position":0}]`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Missing Access","code":50001}`))
	}))
	defer srv.Close()
	out := t.TempDir()
	if err := Run(context.Background(), testCfg(t, srv.URL, out)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(channelDir(out)); !os.IsNotExist(err) {
		t.Fatalf("empty/no-access channel dir should not exist: %v", err)
	}
	index, err := os.ReadFile(filepath.Join(out, "server_G", "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(index)) != "[]" {
		t.Fatalf("index should be empty: %s", index)
	}
}
