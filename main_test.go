package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"discord-backup/internal/config"
)

// TestProbeAddr pins the healthcheck's address handling: a wildcard bind
// address must be probed on loopback, a real host must be kept.
func TestProbeAddr(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:8765": "127.0.0.1:8765",
		"0.0.0.0:8765":   "127.0.0.1:8765",
		":8765":          "127.0.0.1:8765",
		"[::]:8765":      "127.0.0.1:8765",
		"[::1]:8765":     "[::1]:8765",
		"example.com:9":  "example.com:9",
		"no-port":        "no-port",
	} {
		if got := probeAddr(in); got != want {
			t.Errorf("probeAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIsLoopbackAddr covers the warning that stops the "published port is dead"
// trap: a loopback bind inside a container is unreachable from the host.
func TestIsLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8765": true,
		"localhost:8765": true,
		"[::1]:8765":     true,
		"0.0.0.0:8765":   false,
		":8765":          false,
		"[::]:8765":      false,
		"no-port":        false,
	} {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

// TestMCPSmoke covers the authenticated round trip against a stub endpoint,
// including the 401 path that tells a user their token is wrong.
func TestMCPSmoke(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-abc" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(string(body), `"initialize"`):
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18",
				"serverInfo":{"name":"discord-backup","version":"test"}}}`)
		case strings.Contains(string(body), `"tools/list"`):
			io.WriteString(w, `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"whoami"},{"name":"list_channels"}]}}`)
		default:
			io.WriteString(w, `{"jsonrpc":"2.0","id":3,"result":{"isError":false,
				"structuredContent":{"principal":"alice","discord_id":"U1","visible_channels":2,
				"visible_threads":1,"visible_messages":42,"permission_source":"live"}}}`)
		}
	}))
	defer srv.Close()

	got, err := mcpSmoke(srv.Client(), srv.URL, "tok-abc")
	if err != nil {
		t.Fatalf("smoke: %v", err)
	}
	if got.Principal != "alice" || got.DiscordID != "U1" || got.Channels != 2 || got.Messages != 42 {
		t.Fatalf("whoami not parsed: %+v", got)
	}
	if got.Protocol != "2025-06-18" || got.Server != "discord-backup" || len(got.Tools) != 2 {
		t.Fatalf("handshake not parsed: %+v", got)
	}

	if _, err := mcpSmoke(srv.Client(), srv.URL, "wrong"); !errors.Is(err, errUnauthorized) {
		t.Fatalf("wrong token: want errUnauthorized, got %v", err)
	}
}

// TestPickPrincipal: a name (or alias/email/id) resolves, "auto" takes the first
// principal that can actually authenticate.
func TestPickPrincipal(t *testing.T) {
	principals := []config.Principal{
		{Name: "alice", DiscordID: "U1", Aliases: []string{"ali"}},
		{Name: "bob", DiscordID: "U2", Token: "tok"},
	}
	got, err := pickPrincipal(principals, "ali")
	if err != nil || got.Name != "alice" {
		t.Fatalf("by alias: %+v %v", got, err)
	}
	got, err = pickPrincipal(principals, "auto")
	if err != nil || got.Name != "bob" {
		t.Fatalf("auto must pick the one with a token: %+v %v", got, err)
	}
	if _, err := pickPrincipal(principals[:1], "auto"); err == nil {
		t.Fatal("auto without any token must fail")
	}
	if _, err := pickPrincipal(principals, "nobody"); err == nil {
		t.Fatal("unknown principal must fail")
	}
}

// TestLockRetry: a busy archive must be re-checked soon, never after the whole
// interval (which can be hours).
func TestLockRetry(t *testing.T) {
	for every, want := range map[time.Duration]time.Duration{
		0:                time.Minute,
		time.Second:      time.Second,
		45 * time.Second: 45 * time.Second,
		30 * time.Minute: time.Minute,
		time.Minute:      time.Minute,
	} {
		if got := lockRetry(every); got != want {
			t.Errorf("lockRetry(%v) = %v, want %v", every, got, want)
		}
	}
}
