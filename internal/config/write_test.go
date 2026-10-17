package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSavePrincipals: --write replaces mcp.principals, keeps everything else,
// and never lowers the file's protection (it holds tokens, and Load refuses a
// token-bearing file that is not 0600).
func TestSavePrincipals(t *testing.T) {
	body := `{
  "guild_id": "123",
  "archive_dir": "/tmp/archive",
  "token": "bot-token",
  "backup": {"every": "30m", "attach_max_mb": 1024, "channels": ["support"]},
  "mcp": {"http_addr": "0.0.0.0:8765", "default_principal": "martin"}
}`
	path := writeConfig(t, "config.json", body, 0o600)

	want := []Principal{{Name: "martin", DiscordID: "U1", Token: "tok-1"}}
	if _, err := SavePrincipals(path, want); err != nil {
		t.Fatalf("save: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600 (the file holds tokens)", fi.Mode().Perm())
	}

	opts, err := Load(path)
	if err != nil {
		t.Fatalf("reload after save: %v", err)
	}
	if len(opts.MCP.Principals) != 1 || opts.MCP.Principals[0].Token != "tok-1" {
		t.Fatalf("principals not written: %+v", opts.MCP.Principals)
	}
	// Nothing else may move: a rewrite that drops the schedule or the archive
	// path would be a disaster for a running backup.
	if opts.Token != "bot-token" || opts.GuildID != "123" || opts.ArchiveDir != "/tmp/archive" {
		t.Fatalf("shared settings changed: %+v", opts)
	}
	if opts.Backup.Every != 30*time.Minute || opts.Backup.AttachMaxMB != 1024 ||
		len(opts.Backup.Channels) != 1 || opts.Backup.Channels[0] != "support" {
		t.Fatalf("backup settings changed: %+v", opts.Backup)
	}
	if opts.MCP.HTTPAddr != "0.0.0.0:8765" || opts.MCP.DefaultPrincipal != "martin" {
		t.Fatalf("mcp settings changed: %+v", opts.MCP)
	}
	if err := opts.CheckMCP(); err != nil {
		t.Fatalf("written config must be usable by the server: %v", err)
	}
}

// TestSavePrincipalsCreatesSection: a config that has no "mcp" object yet (the
// state before the first server start) gets one.
func TestSavePrincipalsCreatesSection(t *testing.T) {
	path := writeConfig(t, "plain.json", `{"guild_id":"123","archive_dir":"/tmp/archive","token":"bot-token"}`, 0o600)
	if _, err := SavePrincipals(path, []Principal{{Name: "alice", DiscordID: "U2"}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	opts, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.MCP.Principals) != 1 || opts.MCP.Principals[0].Name != "alice" {
		t.Fatalf("principals: %+v", opts.MCP.Principals)
	}
	if opts.MCP.HTTPAddr != "" {
		t.Fatalf("http_addr appeared out of nowhere: %q", opts.MCP.HTTPAddr)
	}
}

// TestSavePrincipalsInPlace: a bind-mounted config cannot be replaced through
// a rename ("device or resource busy"), so the write has to fall back to
// rewriting the file itself - keeping its mode and leaving no staging file
// behind.
func TestSavePrincipalsInPlace(t *testing.T) {
	path := writeConfig(t, "config.json", `{"guild_id":"1","archive_dir":"/a","token":"t"}`, 0o600)

	old := renameFile
	renameFile = func(string, string) error { return errors.New("device or resource busy") }
	t.Cleanup(func() { renameFile = old })

	inPlace, err := SavePrincipals(path, []Principal{{Name: "alice", DiscordID: "U1", Token: "tok"}})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !inPlace {
		t.Fatal("a rename that fails must fall back to an in-place write")
	}
	opts, err := Load(path)
	if err != nil {
		t.Fatalf("reload after in-place write: %v", err)
	}
	if len(opts.MCP.Principals) != 1 || opts.MCP.Principals[0].Token != "tok" {
		t.Fatalf("principals: %+v", opts.MCP.Principals)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only the config", names)
	}
}

// TestSavePrincipalsReadOnlyDir: the image mounts the config into a directory
// it cannot write (its own /etc), where there is no room to stage a file. The
// write still has to go through the file itself.
func TestSavePrincipalsReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"guild_id":"1","archive_dir":"/a","token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) }) // let t.TempDir clean up

	inPlace, err := SavePrincipals(path, []Principal{{Name: "alice", DiscordID: "U1"}})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !inPlace {
		t.Fatal("an unwritable directory must fall back to an in-place write")
	}
	opts, err := Load(path)
	if err != nil {
		t.Fatalf("reload after in-place write: %v", err)
	}
	if len(opts.MCP.Principals) != 1 || opts.MCP.Principals[0].Name != "alice" {
		t.Fatalf("principals: %+v", opts.MCP.Principals)
	}
}

// TestSavePrincipalsMissingFile: the error has to make sense to someone who
// genuinely has no config (the file is the only input).
func TestSavePrincipalsMissingFile(t *testing.T) {
	_, err := SavePrincipals(filepath.Join(t.TempDir(), "nope.json"), nil)
	if err == nil || !strings.Contains(err.Error(), "nope.json") {
		t.Fatalf("want an error naming the file, got %v", err)
	}
}

// TestPrincipalJSONOmitsEmpty: the generated entries stay readable; an empty
// email or aliases list must not turn up as "email": "" / "aliases": null.
func TestPrincipalJSONOmitsEmpty(t *testing.T) {
	path := writeConfig(t, "config.json", `{"guild_id":"1","archive_dir":"/a","token":"t"}`, 0o600)
	if _, err := SavePrincipals(path, []Principal{{Name: "alice", DiscordID: "U2", Token: "tok"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, noise := range []string{`"email"`, `"aliases"`, "null"} {
		if strings.Contains(string(raw), noise) {
			t.Fatalf("config contains %s:\n%s", noise, raw)
		}
	}
	if !strings.Contains(string(raw), `"token": "tok"`) {
		t.Fatalf("token missing:\n%s", raw)
	}
}
