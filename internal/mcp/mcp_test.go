package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"discord-backup/internal/config"
	"discord-backup/internal/discord"
	"discord-backup/internal/perms"
)

// --- fixture ---------------------------------------------------------------

func buildArchive(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	srv := filepath.Join(root, "server_G")
	c1 := filepath.Join(srv, "channel_C1")
	c2 := filepath.Join(srv, "channel_C2")
	t1 := filepath.Join(c1, "threads", "thread_T1")
	for _, d := range []string{c1, c2, t1} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeJSONTo(t, filepath.Join(srv, "server_info.json"), map[string]any{"id": "G", "name": "Guild"})
	writeJSONTo(t, filepath.Join(c1, "channel_info.json"), map[string]any{"id": "C1", "name": "general", "type": 0, "category": "Chat", "message_count": 2})
	writeJSONTo(t, filepath.Join(c2, "channel_info.json"), map[string]any{"id": "C2", "name": "secret", "type": 0, "category": "Chat", "message_count": 1})
	writeJSONTo(t, filepath.Join(t1, "thread_info.json"), map[string]any{"id": "T1", "name": "alpha-thread", "parent_id": "C1", "type": 11, "message_count": 1})

	base := time.Date(2026, 2, 3, 10, 0, 0, 0, time.UTC)
	lines := func(path string, msgs ...string) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		for _, m := range msgs {
			f.WriteString(m + "\n")
		}
	}
	lines(filepath.Join(c1, "messages.jsonl"),
		msgJSON("100", "C1", "hello alpha", "U1", "alice", base),
		msgJSON("200", "C1", "alpha beta", "U2", "bob", base.Add(time.Minute)))
	lines(filepath.Join(c2, "messages.jsonl"),
		msgJSON("300", "C2", "secret alpha", "U2", "bob", base.Add(2*time.Minute)))
	lines(filepath.Join(t1, "messages.jsonl"),
		msgJSON("400", "T1", "thread alpha", "U1", "alice", base.Add(3*time.Minute)))

	// Permission snapshot: @everyone view only, R1 grants read history,
	// R2 is Administrator. C2 denies read history to R1.
	snap := perms.NewSnapshot("G")
	snap.Roles = []discord.Role{
		{ID: "G", Name: "@everyone", Permissions: "1024"},
		{ID: "R1", Name: "readers", Permissions: "65536"},
		{ID: "R2", Name: "admins", Permissions: "8"},
	}
	snap.SetChannels([]discord.Channel{
		{ID: "C1", Name: "general", Type: 0},
		{
			ID: "C2", Name: "secret", Type: 0,
			PermissionOverwrites: []discord.PermissionOverwrite{
				{ID: "R1", Type: 0, Deny: "65536"},
			},
		},
	})
	snap.Members["U1"] = perms.MemberRoles{Username: "alice", Roles: []string{"R1"}}
	snap.Members["U2"] = perms.MemberRoles{Username: "bob"}
	snap.Members["U3"] = perms.MemberRoles{Username: "carol", Roles: []string{"R2"}}
	if err := perms.SaveSnapshot(filepath.Join(srv, "permissions.json"), snap); err != nil {
		t.Fatal(err)
	}
	return root
}

func msgJSON(id, channel, content, authorID, author string, ts time.Time) string {
	obj := map[string]any{
		"id": id, "channel_id": channel, "content": content,
		"timestamp": ts.UTC().Format(time.RFC3339Nano),
		"author":    map[string]any{"id": authorID, "username": author, "bot": false},
	}
	raw, _ := json.Marshal(obj)
	return string(raw)
}

func writeJSONTo(t *testing.T, path string, obj any) {
	t.Helper()
	raw, _ := json.MarshalIndent(obj, "", "  ")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func testSettings(root string) Settings {
	return Settings{
		ArchiveDir: root,
		Principals: []config.Principal{
			{Name: "alice", DiscordID: "U1", Email: "alice@example.com", Aliases: []string{"ali"}, Token: "tok-alice"},
			{Name: "bob", DiscordID: "U2", Token: "tok-bob"},
			{Name: "carol", DiscordID: "U3", Token: "tok-carol"},
		},
	}
}

func newTestServer(t *testing.T, root string) *Server {
	t.Helper()
	s, err := New(context.Background(), testSettings(root), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// toolCall runs a tool and returns its structuredContent, JSON round-tripped
// so nested values are plain maps/slices (like a real MCP client sees).
func toolCall(t *testing.T, s *Server, pr *config.Principal, tool string, args any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(args)
	params, _ := json.Marshal(map[string]any{"name": tool, "arguments": json.RawMessage(raw)})
	resp := s.Handle(context.Background(), pr, &Request{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call", Params: params})
	if resp == nil || resp.Error != nil {
		t.Fatalf("tool %s: bad response %+v", tool, resp)
	}
	enc, _ := json.Marshal(resp.Result)
	var res map[string]any
	if err := json.Unmarshal(enc, &res); err != nil {
		t.Fatal(err)
	}
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("tool %s failed: %v", tool, res["content"])
	}
	sc, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("tool %s: no structuredContent: %#v", tool, res)
	}
	return sc
}

// toolCallErr expects a tool-execution failure.
func toolCallErr(t *testing.T, s *Server, pr *config.Principal, tool string, args any) bool {
	t.Helper()
	raw, _ := json.Marshal(args)
	params, _ := json.Marshal(map[string]any{"name": tool, "arguments": json.RawMessage(raw)})
	resp := s.Handle(context.Background(), pr, &Request{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call", Params: params})
	if resp == nil || resp.Error != nil {
		t.Fatalf("tool %s: bad response %+v", tool, resp)
	}
	enc, _ := json.Marshal(resp.Result)
	var res map[string]any
	if err := json.Unmarshal(enc, &res); err != nil {
		t.Fatal(err)
	}
	isErr, _ := res["isError"].(bool)
	return isErr
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func asList(v any) []any         { l, _ := v.([]any); return l }

// --- protocol --------------------------------------------------------------

func TestInitializeAndToolsList(t *testing.T) {
	s := newTestServer(t, buildArchive(t))

	// The reported version is not a constant any more: the command stamps the
	// build version (YYMM.x) into Version, and initialize is where clients read
	// it, so a detached copy shows up here.
	old := Version
	Version = "2610.1"
	t.Cleanup(func() { Version = old })

	resp := s.Handle(context.Background(), nil, &Request{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "initialize",
		Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{}}`)})
	if resp == nil || resp.Error != nil {
		t.Fatalf("initialize: %+v", resp)
	}
	res := resp.Result.(map[string]any)
	if res["serverInfo"].(map[string]any)["name"] != "discord-backup" {
		t.Fatalf("serverInfo: %+v", res)
	}
	if got := res["serverInfo"].(map[string]any)["version"]; got != "2610.1" {
		t.Fatalf("serverInfo version = %v, want the stamped version", got)
	}

	resp = s.Handle(context.Background(), nil, &Request{JSONRPC: "2.0", ID: json.RawMessage("2"), Method: "tools/list"})
	tools := resp.Result.(map[string]any)["tools"].([]map[string]any)
	if len(tools) != 7 {
		t.Fatalf("expected 7 tools, got %d", len(tools))
	}
	if got := s.Handle(context.Background(), nil, &Request{JSONRPC: "2.0", Method: "notifications/initialized"}); got != nil {
		t.Fatalf("notification must not produce a response, got %+v", got)
	}
	resp = s.Handle(context.Background(), nil, &Request{JSONRPC: "2.0", ID: json.RawMessage("3"), Method: "nope"})
	if resp == nil || resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("expected -32601, got %+v", resp)
	}
}

// --- permission scoping ----------------------------------------------------

func TestWhoamiScoping(t *testing.T) {
	s := newTestServer(t, buildArchive(t))

	for _, tc := range []struct {
		name           string
		channels, msgs float64
	}{
		{"alice", 1, 3}, // C1 + thread, C2 denied
		{"bob", 0, 0},   // no read history
		{"carol", 2, 4}, // admin
	} {
		pr, err := s.settings.Resolve(tc.name)
		if err != nil {
			t.Fatal(err)
		}
		got := toolCall(t, s, pr, "whoami", map[string]any{})
		if got["permission_source"] != "snapshot-only" {
			t.Fatalf("%s: expected snapshot-only, got %v", tc.name, got["permission_source"])
		}
		if got["visible_channels"].(float64) != tc.channels || got["visible_messages"].(float64) != tc.msgs {
			t.Fatalf("%s: channels=%v messages=%v (want %v/%v)",
				tc.name, got["visible_channels"], got["visible_messages"], tc.channels, tc.msgs)
		}
	}
}

func TestSearchRespectsPermissions(t *testing.T) {
	s := newTestServer(t, buildArchive(t))
	alice, _ := s.settings.Resolve("alice")
	got := toolCall(t, s, alice, "search_messages", map[string]any{"query": "alpha"})
	if got["total"].(float64) != 3 {
		t.Fatalf("alice should match 3, got %v", got["total"])
	}
	for _, h := range asList(got["results"]) {
		if asMap(h)["channel_id"] == "C2" {
			t.Fatalf("alice must not see C2 content: %+v", h)
		}
	}

	bob, _ := s.settings.Resolve("bob")
	if got := toolCall(t, s, bob, "search_messages", map[string]any{"query": "alpha"}); got["total"].(float64) != 0 {
		t.Fatalf("bob must see nothing, got %v", got["total"])
	}

	carol, _ := s.settings.Resolve("carol")
	if got := toolCall(t, s, carol, "search_messages", map[string]any{"query": "secret"}); got["total"].(float64) != 1 {
		t.Fatalf("carol should see the secret message, got %v", got["total"])
	}
}

func TestGetMessageDeniedOutsideScope(t *testing.T) {
	s := newTestServer(t, buildArchive(t))
	alice, _ := s.settings.Resolve("alice")

	if !toolCallErr(t, s, alice, "get_message", map[string]any{"message_id": "300"}) {
		t.Fatal("alice must not read a denied message")
	}
	got := toolCall(t, s, alice, "get_message", map[string]any{"message_id": "100"})
	if asMap(got["message"])["content"] != "hello alpha" {
		t.Fatalf("unexpected message: %+v", got)
	}
	if got["raw"] == nil {
		t.Fatal("raw payload missing")
	}
}

func TestListChannelsAndThreads(t *testing.T) {
	s := newTestServer(t, buildArchive(t))
	alice, _ := s.settings.Resolve("alice")

	got := toolCall(t, s, alice, "list_channels", map[string]any{})
	chans := asList(got["channels"])
	if len(chans) != 1 || asMap(chans[0])["id"] != "C1" {
		t.Fatalf("alice channels: %+v", chans)
	}
	if asMap(chans[0])["thread_count"].(float64) != 1 {
		t.Fatalf("expected a thread count of 1: %+v", chans[0])
	}

	got = toolCall(t, s, alice, "list_threads", map[string]any{"channel": "#general"})
	threads := asList(got["threads"])
	if len(threads) != 1 || asMap(threads[0])["id"] != "T1" {
		t.Fatalf("threads: %+v", threads)
	}
}

func TestGetContext(t *testing.T) {
	s := newTestServer(t, buildArchive(t))
	alice, _ := s.settings.Resolve("alice")
	got := toolCall(t, s, alice, "get_context", map[string]any{"message_id": "200", "before": 1, "after": 1})
	msgs := asList(got["messages"])
	if len(msgs) != 2 || asMap(msgs[0])["id"] != "100" || asMap(msgs[1])["id"] != "200" {
		t.Fatalf("context: %+v", msgs)
	}
}

func TestListAuthors(t *testing.T) {
	s := newTestServer(t, buildArchive(t))
	carol, _ := s.settings.Resolve("carol")
	got := toolCall(t, s, carol, "list_authors", map[string]any{})
	if len(asList(got["authors"])) == 0 {
		t.Fatal("expected authors")
	}
}

// --- identity --------------------------------------------------------------

func TestResolvePrincipals(t *testing.T) {
	st := testSettings(buildArchive(t))
	for _, id := range []string{"alice", "ALICE", "alice@example.com", "ali", "U1"} {
		p, err := st.Resolve(id)
		if err != nil || p.DiscordID != "U1" {
			t.Fatalf("resolve %q: %+v %v", id, p, err)
		}
	}
	if _, err := st.Resolve("nobody"); err == nil {
		t.Fatal("unknown principal must fail")
	}
	// Several principals: an explicit one is required.
	if _, err := st.StdioPrincipal(""); err == nil {
		t.Fatal("multiple principals must require an explicit --principal")
	}
	if p, err := st.StdioPrincipal("carol"); err != nil || p.DiscordID != "U3" {
		t.Fatalf("explicit principal: %+v %v", p, err)
	}
	// A single principal needs no explicit choice.
	one := Settings{Principals: st.Principals[:1]}
	if p, err := one.StdioPrincipal(""); err != nil || p.Name != "alice" {
		t.Fatalf("single-principal default: %+v %v", p, err)
	}
	if p, ok := st.ByToken("tok-carol"); !ok || p.DiscordID != "U3" {
		t.Fatalf("ByToken: %+v %v", p, ok)
	}
	if _, ok := st.ByToken("wrong"); ok {
		t.Fatal("wrong token must not resolve")
	}
}

// --- http transport --------------------------------------------------------

func TestHTTPTransport(t *testing.T) {
	s := newTestServer(t, buildArchive(t))
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer nope")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong token, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ = http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok-alice")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Error != nil {
		t.Fatalf("unexpected rpc error: %+v", out.Error)
	}

	req, _ = http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok-alice")
	req.Header.Set("Accept", "text/event-stream")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected SSE content type, got %q", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(raw, []byte("event: message")) || !bytes.Contains(raw, []byte("tools")) {
		t.Fatalf("unexpected SSE body: %s", raw)
	}
}

func TestHTTPBatch(t *testing.T) {
	s := newTestServer(t, buildArchive(t))
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	batch := fmt.Sprintf(`[%s,%s]`,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(batch))
	req.Header.Set("Authorization", "Bearer tok-alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(out))
	}
}
