package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// --- fixture ---------------------------------------------------------------

type fixture struct {
	dir     string
	channel string
	thread  string
}

func msgLine(id, content, authorID, author string, ts time.Time, attach ...string) string {
	atts := make([]map[string]any, 0, len(attach))
	for i, name := range attach {
		atts = append(atts, map[string]any{
			"id": fmt.Sprintf("%s-att-%d", id, i), "filename": name,
			"size": 1, "url": "https://cdn.discordapp.com/" + name,
		})
	}
	obj := map[string]any{
		"id": id, "channel_id": "C", "content": content,
		"timestamp":   ts.UTC().Format(time.RFC3339Nano),
		"author":      map[string]any{"id": authorID, "username": author, "bot": false},
		"attachments": atts,
	}
	raw, _ := json.Marshal(obj)
	return string(raw)
}

func buildFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	srv := filepath.Join(root, "server_G")
	cdir := filepath.Join(srv, "channel_C")
	tdir := filepath.Join(cdir, "threads", "thread_T")
	for _, d := range []string{cdir, tdir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeJSONFile(t, filepath.Join(srv, "server_info.json"), map[string]any{"id": "G", "name": "Test Guild"})
	writeJSONFile(t, filepath.Join(cdir, "channel_info.json"), map[string]any{
		"id": "C", "name": "general", "type": 0, "category": "Chat", "message_count": 3,
	})
	writeJSONFile(t, filepath.Join(tdir, "thread_info.json"), map[string]any{
		"id": "T", "name": "topic", "parent_id": "C", "type": 11, "message_count": 1,
	})

	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	msgs := []string{
		msgLine("100", "hello world", "U1", "alice", base),
		msgLine("200", "the budget report is attached", "U2", "bob", base.Add(time.Minute), "budget-2026.pdf"),
		msgLine("300", "lunch plans", "U1", "alice", base.Add(2*time.Minute)),
	}
	appendLines(t, filepath.Join(cdir, "messages.jsonl"), msgs...)
	// One edit: message 100 gets a new version.
	appendLines(t, filepath.Join(cdir, "edits.jsonl"),
		msgLine("100", "hello wonderful world", "U1", "alice", base))
	// One delete: message 300.
	appendLines(t, filepath.Join(cdir, "deletes.jsonl"),
		`{"id":"300","detected_at":"2026-01-02T03:10:05Z"}`)
	// A thread message.
	appendLines(t, filepath.Join(tdir, "messages.jsonl"),
		msgLine("150", "thread reply about budget", "U2", "bob", base.Add(30*time.Second)))

	return fixture{dir: srv, channel: cdir, thread: tdir}
}

func writeJSONFile(t *testing.T, path string, obj any) {
	t.Helper()
	raw, _ := json.MarshalIndent(obj, "", "  ")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// --- tests -----------------------------------------------------------------

func TestLoadAndResolve(t *testing.T) {
	fx := buildFixture(t)
	a, err := Open(context.Background(), Options{Dir: fx.dir})
	if err != nil {
		t.Fatal(err)
	}
	if a.GuildID() != "G" || a.Name() != "Test Guild" {
		t.Fatalf("meta: %s / %s", a.GuildID(), a.Name())
	}
	logs, msgs := a.Count(AllowAll())
	if logs != 2 || msgs != 3 {
		t.Fatalf("count: logs=%d msgs=%d (want 2/3, message 300 is deleted)", logs, msgs)
	}
	// Edit applied.
	d, raw, ok := a.Message("100", AllowAll())
	if !ok || d.Content != "hello wonderful world" || !d.Edited {
		t.Fatalf("edit not applied: %+v ok=%v", d, ok)
	}
	if len(raw) == 0 {
		t.Fatal("raw payload missing")
	}
	// Deleted message hidden by default.
	if _, _, ok := a.Message("300", AllowAll()); ok {
		t.Fatal("deleted message must be hidden by default")
	}
	// Thread message inherits parent channel access.
	if _, _, ok := a.Message("150", ChannelAccess(map[string]bool{"C": true}, nil)); !ok {
		t.Fatal("thread message should be visible with parent access")
	}
	if _, _, ok := a.Message("150", ChannelAccess(map[string]bool{"X": true}, nil)); ok {
		t.Fatal("thread message must be hidden without parent access")
	}
}

func TestShowDeleted(t *testing.T) {
	fx := buildFixture(t)
	a, err := Open(context.Background(), Options{Dir: fx.dir, ShowDeleted: true})
	if err != nil {
		t.Fatal(err)
	}
	d, _, ok := a.Message("300", AllowAll())
	if !ok || !d.Deleted {
		t.Fatal("deleted message should be visible and flagged when ShowDeleted")
	}
}

func TestSearch(t *testing.T) {
	fx := buildFixture(t)
	a, err := Open(context.Background(), Options{Dir: fx.dir})
	if err != nil {
		t.Fatal(err)
	}
	access := AllowAll()

	// Term match finds the edited content.
	hits, total := a.Search(access, Query{Terms: []string{"wonderful"}})
	if total != 1 || len(hits) != 1 || hits[0].MessageID != "100" {
		t.Fatalf("search 'wonderful': %+v total=%d", hits, total)
	}
	if hits[0].Edited != true || hits[0].Channel != "general" {
		t.Fatalf("hit metadata: %+v", hits[0])
	}

	// Attachment filename is searchable.
	hits, _ = a.Search(access, Query{Terms: []string{"budget-2026"}})
	if len(hits) != 1 || hits[0].MessageID != "200" || !hits[0].Attach {
		t.Fatalf("attachment search: %+v", hits)
	}

	// Deleted content is not searchable by default.
	if hits, _ := a.Search(access, Query{Terms: []string{"lunch"}}); len(hits) != 0 {
		t.Fatalf("deleted content leaked into search: %+v", hits)
	}

	// Author filter.
	hits, _ = a.Search(access, Query{Author: "alice"})
	if len(hits) != 1 || hits[0].MessageID != "100" {
		t.Fatalf("author filter: %+v", hits)
	}

	// Channel filter selects only the thread.
	hits, _ = a.Search(access, Query{Terms: []string{"budget"}, Channel: "topic"})
	if len(hits) != 1 || hits[0].Kind != KindThread {
		t.Fatalf("channel filter: %+v", hits)
	}

	// Ordering: newest first by default.
	hits, _ = a.Search(access, Query{})
	if len(hits) < 2 || hits[0].TS.Before(hits[1].TS) {
		t.Fatalf("expected newest-first ordering: %+v", hits)
	}
}

func TestSearchAccessScoped(t *testing.T) {
	fx := buildFixture(t)
	a, err := Open(context.Background(), Options{Dir: fx.dir})
	if err != nil {
		t.Fatal(err)
	}
	// Only the thread's parent channel is not allowed -> nothing.
	hits, total := a.Search(ChannelAccess(map[string]bool{}, nil), Query{})
	if total != 0 || len(hits) != 0 {
		t.Fatalf("expected no hits without access, got %d", total)
	}
}

func TestContext(t *testing.T) {
	fx := buildFixture(t)
	a, err := Open(context.Background(), Options{Dir: fx.dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, ok := a.MessageContext("200", AllowAll(), 1, 1)
	if !ok {
		t.Fatal("context not found")
	}
	// 100, 200 (deleted 300 is hidden).
	if len(ctx) != 2 || ctx[0].ID != "100" || ctx[1].ID != "200" {
		t.Fatalf("context: %v", ids(ctx))
	}
}

func ids(docs []MessageInfo) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.ID
	}
	return out
}

func TestIncrementalRefresh(t *testing.T) {
	fx := buildFixture(t)
	a, err := Open(context.Background(), Options{Dir: fx.dir})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 2, 4, 0, 0, 0, time.UTC)
	appendLines(t, filepath.Join(fx.channel, "messages.jsonl"),
		msgLine("400", "a brand new message", "U1", "alice", base))
	// Also a brand new thread.
	ndir := filepath.Join(fx.channel, "threads", "thread_T2")
	os.MkdirAll(ndir, 0o755)
	writeJSONFile(t, filepath.Join(ndir, "thread_info.json"),
		map[string]any{"id": "T2", "name": "fresh", "parent_id": "C", "type": 11})
	appendLines(t, filepath.Join(ndir, "messages.jsonl"),
		msgLine("500", "fresh thread content", "U2", "bob", base))

	if err := a.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := a.Message("400", AllowAll()); !ok {
		t.Fatal("new message not picked up by refresh")
	}
	if _, _, ok := a.Message("500", AllowAll()); !ok {
		t.Fatal("new thread not picked up by refresh")
	}
	hits, _ := a.Search(AllowAll(), Query{Terms: []string{"fresh"}})
	if len(hits) != 1 || hits[0].Channel != "fresh" {
		t.Fatalf("new thread not searchable: %+v", hits)
	}
	// Refresh must not duplicate existing docs.
	logs, msgs := a.Count(AllowAll())
	if logs != 3 || msgs != 5 {
		t.Fatalf("after refresh: logs=%d msgs=%d (want 3/5)", logs, msgs)
	}
}

func TestThreadTypeSelfHeals(t *testing.T) {
	fx := buildFixture(t)
	a, err := Open(context.Background(), Options{Dir: fx.dir})
	if err != nil {
		t.Fatal(err)
	}
	// A thread archived by an older version: no type recorded.
	ndir := filepath.Join(fx.channel, "threads", "thread_T9")
	if err := os.MkdirAll(ndir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(ndir, "thread_info.json"),
		map[string]any{"id": "T9", "name": "old", "parent_id": "C"})
	appendLines(t, filepath.Join(ndir, "messages.jsonl"),
		msgLine("600", "old thread content", "U1", "alice", time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)))

	access := ChannelAccess(map[string]bool{"C": true}, nil) // private threads hidden
	if _, _, ok := a.Message("600", access); ok {
		t.Fatal("a thread without a recorded type must be hidden")
	}

	// The next backup run records the type; the running reader picks it up.
	writeJSONFile(t, filepath.Join(ndir, "thread_info.json"),
		map[string]any{"id": "T9", "name": "old", "parent_id": "C", "type": 11})
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := a.Message("600", access); !ok {
		t.Fatal("thread type was not picked up on refresh")
	}
}

func TestPartialTailIgnored(t *testing.T) {
	fx := buildFixture(t)
	a, err := Open(context.Background(), Options{Dir: fx.dir})
	if err != nil {
		t.Fatal(err)
	}
	// Append a torn (unterminated) line: must be ignored, not crash.
	f, err := os.OpenFile(filepath.Join(fx.channel, "messages.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"id":"999","content":"torn`)
	f.Close()

	if err := a.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := a.Message("999", AllowAll()); ok {
		t.Fatal("torn line must not be visible")
	}
	// Completing the line makes it visible.
	appendLines(t, filepath.Join(fx.channel, "messages.jsonl"), "") // just a newline closes the torn line
	// The torn line is now a complete (but invalid) JSON line -> skipped.
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := a.Message("999", AllowAll()); ok {
		t.Fatal("invalid line must not be visible")
	}
}

// TestLogRewriteReloads covers the backup's history heal: PrependLines
// rewrites messages.jsonl via temp file + rename, which shifts every stored
// byte offset. The reader must detect that and rebuild the log.
func TestLogRewriteReloads(t *testing.T) {
	fx := buildFixture(t)
	a, err := Open(context.Background(), Options{Dir: fx.dir})
	if err != nil {
		t.Fatal(err)
	}
	// Read once so the reader stores offsets past 0.
	a.Count(AllowAll())

	path := filepath.Join(fx.channel, "messages.jsonl")
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	older := msgLine("50", "a much older note", "U1", "alice",
		time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC))
	if err := os.WriteFile(path+".rewrite", append([]byte(older+"\n"), old...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".rewrite", path); err != nil {
		t.Fatal(err)
	}

	if err := a.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := a.Message("50", AllowAll()); !ok {
		t.Fatal("prepended message not picked up after a rewrite")
	}
	// The original messages survive, the edit is still applied, and nothing
	// is duplicated.
	d, _, ok := a.Message("100", AllowAll())
	if !ok || d.Content != "hello wonderful world" {
		t.Fatalf("existing message lost or edit dropped after rewrite: %+v ok=%v", d, ok)
	}
	logs, msgs := a.Count(AllowAll())
	if logs != 2 || msgs != 4 {
		t.Fatalf("after rewrite: logs=%d msgs=%d (want 2/4)", logs, msgs)
	}
}

// TestAppendDoesNotLookLikeRewrite guards against a naive head check that
// treats normal append-only growth as a rewrite (which would force a full
// reload on every refresh for small files).
func TestAppendDoesNotLookLikeRewrite(t *testing.T) {
	fx := buildFixture(t)
	a, err := Open(context.Background(), Options{Dir: fx.dir})
	if err != nil {
		t.Fatal(err)
	}
	var mfs *fileState
	for _, f := range a.files {
		if f.log.ID == "C" && f.kind == "messages" {
			mfs = f
		}
	}
	if mfs == nil {
		t.Fatal("messages fileState not registered")
	}
	if mfs.off == 0 || mfs.headLen == 0 {
		t.Fatalf("expected a recorded offset and head, got off=%d headLen=%d", mfs.off, mfs.headLen)
	}
	headLen := mfs.headLen
	appendLines(t, filepath.Join(fx.channel, "messages.jsonl"),
		msgLine("700", "just another note", "U1", "alice", time.Date(2026, 1, 2, 5, 0, 0, 0, time.UTC)))
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mfs.headLen != headLen {
		t.Fatalf("an append must not change the head window: %d -> %d", headLen, mfs.headLen)
	}
	if _, _, ok := a.Message("700", AllowAll()); !ok {
		t.Fatal("appended message not picked up")
	}
}

// TestConcurrentReadDuringRefresh must run clean under -race: readers take
// value snapshots, so a concurrent Refresh never mutates what they hold.
func TestConcurrentReadDuringRefresh(t *testing.T) {
	fx := buildFixture(t)
	a, err := Open(context.Background(), Options{Dir: fx.dir})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				a.Search(AllowAll(), Query{Terms: []string{"world"}})
				a.Count(AllowAll())
				a.Authors(AllowAll(), nil, 5)
				a.LogsInfo(AllowAll())
				if info, _, ok := a.Message("100", AllowAll()); ok {
					_ = info.Content
				}
				a.MessageContext("200", AllowAll(), 1, 1)
			}
		}()
	}
	for i := 0; i < 25; i++ {
		if err := a.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestAuthors(t *testing.T) {
	fx := buildFixture(t)
	a, err := Open(context.Background(), Options{Dir: fx.dir})
	if err != nil {
		t.Fatal(err)
	}
	counts := a.Authors(AllowAll(), nil, 10)
	if len(counts) != 2 {
		t.Fatalf("authors: %+v", counts)
	}
	if counts[0].Author != "bob" || counts[0].Messages != 2 {
		t.Fatalf("expected bob first with 2 messages: %+v", counts)
	}
}
