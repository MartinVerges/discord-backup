package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"discord-backup/internal/config"
	"discord-backup/internal/discord"
)

// stubAPI serves the guild, its member pages, its channels and its roles - the
// four things the principals command may ask for. page receives the `after`
// cursor and returns that page of members.
func stubAPI(t *testing.T, page func(after string) []map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/members"):
			json.NewEncoder(w).Encode(page(r.URL.Query().Get("after")))
		case strings.HasSuffix(r.URL.Path, "/channels"):
			// #general: everyone may view it, READ_MESSAGE_HISTORY needs R1.
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": "C1", "name": "general", "type": 0},
				{"id": "C2", "name": "secret", "type": 0, "permission_overwrites": []map[string]any{
					{"id": "G", "type": 0, "allow": "0", "deny": "1024"},
				}},
			})
		case strings.HasSuffix(r.URL.Path, "/roles"):
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": "G", "name": "@everyone", "permissions": "1024", "position": 0},
				{"id": "R1", "name": "readers", "permissions": "65536", "position": 1},
			})
		case strings.HasPrefix(r.URL.Path, "/guilds/"):
			io.WriteString(w, `{"id":"G","name":"Test Guild","approximate_member_count":3}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// stubDiscord pages 1000 members, then one more. With loop it answers every
// page with the same 1000 members - the shape of a broken API or a proxy -
// which must end the walk, not spin forever.
func stubDiscord(t *testing.T, loop bool) *httptest.Server {
	t.Helper()
	bigPage := make([]map[string]any, 0, 1000)
	for i := 0; i < 1000; i++ {
		bigPage = append(bigPage, memberJSON(fmt.Sprintf("m%04d", i), fmt.Sprintf("user%04d", i)))
	}
	return stubAPI(t, func(after string) []map[string]any {
		switch {
		case loop, after == "":
			return bigPage
		case after == "m0999":
			return []map[string]any{memberJSON("m1000", "zed")}
		default:
			return bigPage
		}
	})
}

func memberJSON(id, username string) map[string]any {
	return map[string]any{"user": map[string]any{"id": id, "username": username}, "roles": []string{}}
}

// writeMCPConfig writes a token-bearing config with the mode Load insists on.
func writeMCPConfig(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	body := fmt.Sprintf(`{"guild_id":"G","archive_dir":%q,"token":"tok","mcp":{"deleted":"hide"%s}}`,
		t.TempDir(), extra)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// captureStdout runs fn with stdout redirected. The reader runs in a goroutine:
// the block for 1000 members is larger than a pipe buffer, so writing would
// otherwise deadlock before anyone reads it.
func captureStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		buf, _ := io.ReadAll(r)
		done <- string(buf)
	}()
	code := fn()
	w.Close()
	os.Stdout = old
	out := <-done
	r.Close()
	return out, code
}

// TestRunPrincipalsEndToEnd walks the whole command against a stub API: two
// pages of members and the printed block, then --write, then a second run that
// must change nothing at all.
func TestRunPrincipalsEndToEnd(t *testing.T) {
	srv := stubDiscord(t, false)
	old := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = old })

	path := writeMCPConfig(t, "")
	out, code := captureStdout(t, func() int {
		return runPrincipals([]string{"--config", path, "--include-bots"})
	})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, `"name": "user0000"`) || !strings.Contains(out, `"name": "zed"`) {
		t.Fatalf("block misses members:\n%s", out)
	}
	if n := strings.Count(out, `"discord_id"`); n != 1001 {
		t.Fatalf("block has %d principals, want 1001 (pagination or dedup is off)", n)
	}

	// --write: the file is the result, stdout carries the token table.
	out, code = captureStdout(t, func() int {
		return runPrincipals([]string{"--config", path, "--include-bots", "--write"})
	})
	if code != 0 {
		t.Fatalf("--write exit %d", code)
	}
	if !strings.HasPrefix(out, "NAME") || !strings.Contains(out, "user0000") {
		t.Fatalf("token table:\n%s", out)
	}
	opts, err := config.Load(path)
	if err != nil {
		t.Fatalf("config after --write: %v", err)
	}
	if len(opts.MCP.Principals) != 1001 {
		t.Fatalf("wrote %d principals, want 1001", len(opts.MCP.Principals))
	}
	if err := opts.CheckMCP(); err != nil {
		t.Fatalf("written config must be usable: %v", err)
	}
	firstToken := opts.MCP.Principals[0].Token

	// Idempotent: a second run adds nobody and keeps every token.
	if _, code = captureStdout(t, func() int {
		return runPrincipals([]string{"--config", path, "--include-bots", "--write"})
	}); code != 0 {
		t.Fatalf("second --write exit %d", code)
	}
	again, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.MCP.Principals) != 1001 || again.MCP.Principals[0].Token != firstToken {
		t.Fatalf("second run changed the principals (%d entries)", len(again.MCP.Principals))
	}
}

// TestRunPrincipalsNoToken: the objects can be printed without tokens (stdio
// needs none), and the block stays pasteable.
func TestRunPrincipalsNoToken(t *testing.T) {
	srv := stubDiscord(t, false)
	old := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = old })

	path := writeMCPConfig(t, "")
	out, code := captureStdout(t, func() int {
		return runPrincipals([]string{"--config", path, "--no-token"})
	})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(out, `"token"`) {
		t.Fatalf("--no-token still emitted a token:\n%s", out)
	}
	if !strings.Contains(out, `"name": "user0000"`) {
		t.Fatalf("no principals:\n%s", out)
	}
}

// TestRunPrincipalsFromChannel keeps only the members who may actually read the
// channel: @everyone may view #general, but reading its history needs the role.
func TestRunPrincipalsFromChannel(t *testing.T) {
	srv := stubAPI(t, func(string) []map[string]any {
		return []map[string]any{
			{"user": map[string]any{"id": "U1", "username": "aduffeck_63702", "global_name": "Alexander Duffeck"}, "roles": []string{"R1"}},
			{"user": map[string]any{"id": "U2", "username": "bob"}, "roles": []string{}},
			{"user": map[string]any{"id": "U3", "username": "carol"}, "roles": []string{"R1"}},
		}
	})
	old := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = old })
	path := writeMCPConfig(t, "")

	out, code := captureStdout(t, func() int {
		return runPrincipals([]string{"--config", path, "--from-channel", "#general"})
	})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "Alexander Duffeck") || !strings.Contains(out, "carol") {
		t.Fatalf("readers missing:\n%s", out)
	}
	if strings.Contains(out, "bob") {
		t.Fatalf("bob may not read #general, but is in the list:\n%s", out)
	}

	// A channel @everyone cannot even see is the other direction, and it works
	// by ID too: nobody is a member of #secret.
	out, code = captureStdout(t, func() int {
		return runPrincipals([]string{"--config", path, "--from-channel", "C2"})
	})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(out, `"discord_id"`) {
		t.Fatalf("nobody may read #secret:\n%s", out)
	}

	// An unknown channel must fail instead of quietly keeping everybody.
	if _, code := captureStdout(t, func() int {
		return runPrincipals([]string{"--config", path, "--from-channel", "nope"})
	}); code == 0 {
		t.Fatal("an unknown channel must fail")
	}
}

// TestListAllMembersStopsOnRepeatPage: the guard that keeps a broken API from
// turning into an endless loop of requests.
func TestListAllMembersStopsOnRepeatPage(t *testing.T) {
	srv := stubDiscord(t, true)
	got, err := listAllMembers(t.Context(), discord.New("tok", srv.URL), "G")
	if err != nil {
		t.Fatal(err)
	}
	// Two pages, then the cursor stops advancing: 1000 + 1000.
	if len(got) != 2000 {
		t.Fatalf("got %d members, want 2000 (the repeat page must end the walk)", len(got))
	}
}
