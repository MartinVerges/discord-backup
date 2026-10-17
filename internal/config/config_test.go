package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig writes a config with an explicit mode (defeating umask).
func writeConfig(t *testing.T, name, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestExampleConfigParses keeps the shipped config.example.json valid: every
// key it uses must exist, its values must pass validation, and both commands
// must accept it as-is (so copying it and adding a token really works).
func TestExampleConfigParses(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	o, err := Parse(raw)
	if err != nil {
		t.Fatalf("config.example.json does not parse: %v", err)
	}
	if err := o.CheckBackup(); err != nil {
		t.Fatalf("config.example.json is not a valid backup config: %v", err)
	}
	if err := o.CheckMCP(); err != nil {
		t.Fatalf("config.example.json is not a valid MCP config: %v", err)
	}
}

func TestLoadDefaults(t *testing.T) {
	p := writeConfig(t, "config.json", `{
		"guild_id": "G",
		"archive_dir": "/data",
		"token": "bot-secret",
		"mcp": {"principals": [{"name": "alice", "discord_id": "U1"}]}
	}`, 0o600)
	o, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if o.GuildID != "G" || o.ArchiveDir != "/data" || o.Token != "bot-secret" {
		t.Fatalf("top level not parsed: %+v", o)
	}
	// Backup defaults: skip_empty on, 48h edit window, no schedule, 50 MB / 4.
	if !o.Backup.SkipEmpty || o.Backup.EditWindow != 48*time.Hour || o.Backup.Every != 0 {
		t.Fatalf("backup defaults wrong: %+v", o.Backup)
	}
	if o.Backup.AttachMaxMB != 50 || o.Backup.AttachWorkers != 4 {
		t.Fatalf("attachment defaults wrong: %+v", o.Backup)
	}
	// MCP defaults.
	if o.MCP.ShowDeleted || o.MCP.RefreshTTL != time.Minute || o.MCP.ChannelTTL != 5*time.Minute {
		t.Fatalf("mcp defaults wrong: %+v", o.MCP)
	}
	if o.MCP.RetryTTL != 5*time.Minute || o.MCP.Timeout != 5*time.Second {
		t.Fatalf("mcp defaults wrong: %+v", o.MCP)
	}
}

func TestLoadValues(t *testing.T) {
	p := writeConfig(t, "config.json", `{
		"guild_id": "G",
		"archive_dir": "/data",
		"token": "bot-secret",
		"backup": {
			"download_attachments": true,
			"include_bots": true,
			"skip_empty": false,
			"edit_window": "0",
			"attach_max_mb": 10,
			"attach_workers": 2,
			"every": "15m",
			"channels": ["general", "123"]
		},
		"mcp": {
			"deleted": "show",
			"channel_ttl": "1m",
			"check_private_threads": true,
			"http_addr": "0.0.0.0:8765",
			"principals": [{"name": "alice", "discord_id": "U1", "token": "t1"}]
		}
	}`, 0o600)
	o, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !o.Backup.DownloadAttachments || !o.Backup.IncludeBots || o.Backup.SkipEmpty {
		t.Fatalf("backup bools wrong: %+v", o.Backup)
	}
	if o.Backup.EditWindow != 0 || o.Backup.Every != 15*time.Minute {
		t.Fatalf("backup durations wrong: %+v", o.Backup)
	}
	if len(o.Backup.Channels) != 2 || o.Backup.AttachMaxMB != 10 || o.Backup.AttachWorkers != 2 {
		t.Fatalf("backup values wrong: %+v", o.Backup)
	}
	if !o.MCP.ShowDeleted || o.MCP.ChannelTTL != time.Minute || !o.MCP.CheckPrivateThreads {
		t.Fatalf("mcp values wrong: %+v", o.MCP)
	}
	if o.MCP.HTTPAddr != "0.0.0.0:8765" {
		t.Fatalf("http_addr not parsed: %+v", o.MCP)
	}
}

func TestLoadSecretPermissions(t *testing.T) {
	const withBotToken = `{"guild_id":"G","archive_dir":"/data","token":"bot-secret"}`
	const withPrincipalToken = `{"guild_id":"G","archive_dir":"/data",
		"mcp":{"principals":[{"name":"alice","discord_id":"U1","token":"bearer"}]}}`
	// No bot token and token-less principals: nothing secret to protect.
	const noSecrets = `{"guild_id":"G","archive_dir":"/data",
		"mcp":{"principals":[{"name":"alice","discord_id":"U1"}]}}`

	if _, err := Load(writeConfig(t, "ok.json", withBotToken, 0o600)); err != nil {
		t.Fatalf("0600 config with token: %v", err)
	}
	for name, body := range map[string]string{
		"bot.json":       withBotToken,
		"principal.json": withPrincipalToken,
	} {
		_, err := Load(writeConfig(t, name, body, 0o644))
		if err == nil {
			t.Fatalf("%s: a config holding tokens must be rejected at 0644", name)
		}
		if !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("%s: error should point at the fix, got %v", name, err)
		}
		if !strings.Contains(err.Error(), "uid") {
			t.Fatalf("%s: error should name the ownership trap, got %v", name, err)
		}
	}
	if _, err := Load(writeConfig(t, "plain.json", noSecrets, 0o644)); err != nil {
		t.Fatalf("secret-free config at 0644: %v", err)
	}
}

// TestLoadUnreadableHint covers the other half of the container trap: a 0600
// config owned by someone else cannot be opened at all, and the error must say
// which uid it runs as and how to fix it.
func TestLoadUnreadableHint(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	p := writeConfig(t, "locked.json", `{"guild_id":"G","archive_dir":"/data","token":"t"}`, 0o000)
	_, err := Load(p)
	if err == nil {
		t.Fatal("an unreadable config must fail")
	}
	if !strings.Contains(err.Error(), "permission denied") || !strings.Contains(err.Error(), "uid") {
		t.Fatalf("error should name the uid and the fix, got: %v", err)
	}
}

func TestLoadRejectsBadConfig(t *testing.T) {
	valid := `"guild_id":"G","archive_dir":"/data","token":"t"`
	for name, body := range map[string]string{
		"unknown field":  `{` + valid + `,"nope":1}`,
		"no guild":       `{"archive_dir":"/data","token":"t"}`,
		"no archive":     `{"guild_id":"G","token":"t"}`,
		"bad since":      `{` + valid + `,"backup":{"since":"yesterday"}}`,
		"bad duration":   `{` + valid + `,"backup":{"every":"soon"}}`,
		"bad deleted":    `{` + valid + `,"mcp":{"deleted":"maybe"}}`,
		"duplicate name": `{` + valid + `,"mcp":{"principals":[{"name":"a","discord_id":"1"},{"name":"A","discord_id":"2"}]}}`,
		"shared token":   `{` + valid + `,"mcp":{"principals":[{"name":"a","discord_id":"1","token":"x"},{"name":"b","discord_id":"2","token":"x"}]}}`,
		"no discord_id":  `{` + valid + `,"mcp":{"principals":[{"name":"a"}]}}`,
	} {
		if _, err := Load(writeConfig(t, "bad.json", "{"+body+"}", 0o600)); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}

// TestCommandScopedChecks pins the split between shared validation and the
// per-command checks: a problem that only affects the MCP must not stop the
// backup, and a backup-only problem must not stop the MCP.
func TestCommandScopedChecks(t *testing.T) {
	// http_addr without any principal token: Load is fine (the backup does not
	// care), the MCP refuses.
	p := writeConfig(t, "scoped.json", `{
		"guild_id": "G", "archive_dir": "/data", "token": "t",
		"backup": {"limit": 10, "since": "2026-01-01"},
		"mcp": {"http_addr": "127.0.0.1:1",
		        "principals": [{"name": "a", "discord_id": "1"}]}
	}`, 0o600)
	o, err := Load(p)
	if err != nil {
		t.Fatalf("Load must not enforce command-specific rules: %v", err)
	}
	if err := o.CheckBackup(); err == nil {
		t.Fatal("backup: limit+since must be rejected")
	} else if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("backup error unclear: %v", err)
	}
	if err := o.CheckMCP(); err == nil {
		t.Fatal("mcp: http_addr without a token must be rejected")
	} else if !strings.Contains(err.Error(), "token") {
		t.Fatalf("mcp error unclear: %v", err)
	}

	// The mirror image: an MCP problem must not make the backup fail, and a
	// backup problem must not make the MCP fail.
	p = writeConfig(t, "fine.json", `{
		"guild_id": "G", "archive_dir": "/data", "token": "t",
		"mcp": {"principals": [{"name": "a", "discord_id": "1", "token": "x"}]}
	}`, 0o600)
	if o, err := Load(p); err != nil {
		t.Fatal(err)
	} else if err := o.CheckBackup(); err != nil {
		t.Fatalf("backup must accept a config with an incomplete test of mcp: %v", err)
	} else if err := o.CheckMCP(); err != nil {
		t.Fatalf("mcp must accept a valid mcp section: %v", err)
	}

	// No principals at all: only the MCP minds.
	p = writeConfig(t, "no-principals.json",
		`{"guild_id":"G","archive_dir":"/data","token":"t"}`, 0o600)
	o, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.CheckBackup(); err != nil {
		t.Fatalf("backup must not require principals: %v", err)
	}
	if err := o.CheckMCP(); err == nil {
		t.Fatal("mcp must require at least one principal")
	}
}

func TestPrincipalLookup(t *testing.T) {
	p := []Principal{
		{Name: "alice", DiscordID: "U1", Email: "alice@example.com", Aliases: []string{"ali"}, Token: "tok-alice"},
		{Name: "bob", DiscordID: "U2", Token: "tok-bob"},
	}
	for _, id := range []string{"alice", "ALICE", "alice@example.com", "ali", "U1"} {
		got, err := ResolvePrincipal(p, id)
		if err != nil || got.DiscordID != "U1" {
			t.Fatalf("resolve %q: %+v %v", id, got, err)
		}
	}
	if _, err := ResolvePrincipal(p, "nobody"); err == nil {
		t.Fatal("unknown principal must fail")
	}
	if _, err := ResolvePrincipal(nil, "alice"); err == nil {
		t.Fatal("no principals must fail")
	}
	if got, ok := PrincipalByToken(p, "tok-bob"); !ok || got.DiscordID != "U2" {
		t.Fatalf("PrincipalByToken: %+v %v", got, ok)
	}
	if _, ok := PrincipalByToken(p, "wrong"); ok {
		t.Fatal("wrong token must not resolve")
	}
}
