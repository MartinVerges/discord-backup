package authz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"discord-backup/internal/discord"
	"discord-backup/internal/perms"
)

// ViewChannel only for @everyone; READ_MESSAGE_HISTORY via role R1.
func testRoles() []map[string]any {
	return []map[string]any{
		{"id": "G", "name": "@everyone", "permissions": "1024", "position": 0},
		{"id": "R1", "name": "readers", "permissions": "65536", "position": 1},
	}
}

func testChannels() []map[string]any {
	return []map[string]any{
		{"id": "C", "name": "general", "type": 0, "permission_overwrites": []any{}},
	}
}

type fakeAPI struct {
	fail atomic.Bool
	// memberHits counts calls to the per-member endpoint, so a test can prove
	// a code path does not ask Discord once per member.
	memberHits atomic.Int64
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/guilds/G/channels", func(w http.ResponseWriter, r *http.Request) {
		if f.fail.Load() {
			http.Error(w, "discord down", http.StatusForbidden)
			return
		}
		json.NewEncoder(w).Encode(testChannels())
	})
	mux.HandleFunc("/guilds/G/roles", func(w http.ResponseWriter, r *http.Request) {
		if f.fail.Load() {
			http.Error(w, "discord down", http.StatusForbidden)
			return
		}
		json.NewEncoder(w).Encode(testRoles())
	})
	mux.HandleFunc("/guilds/G/members/", func(w http.ResponseWriter, r *http.Request) {
		f.memberHits.Add(1)
		if f.fail.Load() {
			http.Error(w, "discord down", http.StatusForbidden)
			return
		}
		id := r.URL.Path[len("/guilds/G/members/"):]
		switch id {
		case "U1":
			json.NewEncoder(w).Encode(map[string]any{
				"user": map[string]any{"id": "U1", "username": "alice"}, "roles": []string{"R1"},
			})
		case "U2":
			json.NewEncoder(w).Encode(map[string]any{
				"user": map[string]any{"id": "U2", "username": "bob"}, "roles": []string{},
			})
		default:
			http.Error(w, "unknown member", http.StatusNotFound)
		}
	})
	return mux
}

func snapshotFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "permissions.json")
	snap := perms.NewSnapshot("G")
	snap.Roles = []discord.Role{
		{ID: "G", Permissions: "1024"},
		{ID: "R1", Permissions: "65536"},
	}
	snap.SetChannels([]discord.Channel{{ID: "C", Name: "general", Type: 0}})
	snap.Members["U1"] = perms.MemberRoles{Username: "alice", Roles: []string{"R1"}}
	snap.Members["U2"] = perms.MemberRoles{Username: "bob"}
	if err := perms.SaveSnapshot(path, snap); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOfflineSnapshotOnly(t *testing.T) {
	path := snapshotFile(t)
	r := New(Config{SnapshotPath: path, GuildID: "G"}) // no client

	got, err := r.AllowedChannels(context.Background(), "U1")
	if err != nil {
		t.Fatal(err)
	}
	if !got["C"] {
		t.Fatalf("U1 should read C, got %v", got)
	}
	got, err = r.AllowedChannels(context.Background(), "U2")
	if err != nil {
		t.Fatal(err)
	}
	if got["C"] {
		t.Fatalf("U2 must not read C, got %v", got)
	}
	if got, _ := r.AllowedChannels(context.Background(), "U9"); got["C"] {
		t.Fatalf("unknown user must see nothing, got %v", got)
	}
}

func TestLiveLookup(t *testing.T) {
	fake := &fakeAPI{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	path := snapshotFile(t)
	r := New(Config{
		Client:       discord.New("tok", srv.URL),
		SnapshotPath: path,
		GuildID:      "G",
		ChannelTTL:   time.Minute,
		MemberTTL:    time.Minute,
	})
	got, err := r.AllowedChannels(context.Background(), "U1")
	if err != nil {
		t.Fatal(err)
	}
	if !got["C"] {
		t.Fatalf("U1 should read C via live lookup, got %v", got)
	}
	if got, _ := r.AllowedChannels(context.Background(), "U2"); got["C"] {
		t.Fatalf("U2 must not read C, got %v", got)
	}
}

// TestAllowedChannelsForRoles: a caller that already has the member's roles
// (the principals command reads them from the member list) gets the same answer
// without a single member lookup.
func TestAllowedChannelsForRoles(t *testing.T) {
	fake := &fakeAPI{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	r := New(Config{
		Client:       discord.New("tok", srv.URL),
		SnapshotPath: snapshotFile(t),
		GuildID:      "G",
		ChannelTTL:   time.Minute,
		MemberTTL:    time.Minute,
	})
	ctx := context.Background()
	got, err := r.AllowedChannelsForRoles(ctx, "U1", []string{"R1"})
	if err != nil {
		t.Fatal(err)
	}
	if !got["C"] {
		t.Fatalf("a member with R1 must read C, got %v", got)
	}
	got, err = r.AllowedChannelsForRoles(ctx, "U2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got["C"] {
		t.Fatalf("a member without roles must not read C, got %v", got)
	}
	if n := fake.memberHits.Load(); n != 0 {
		t.Fatalf("%d member lookups, want 0 (the caller knows the roles)", n)
	}
}

func TestFallbackToCacheWhenDiscordDown(t *testing.T) {
	fake := &fakeAPI{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	path := snapshotFile(t)
	r := New(Config{
		Client:       discord.New("tok", srv.URL),
		SnapshotPath: path,
		GuildID:      "G",
		ChannelTTL:   time.Millisecond,
		MemberTTL:    time.Millisecond,
	})
	if got, _ := r.AllowedChannels(context.Background(), "U1"); !got["C"] {
		t.Fatalf("warm-up failed: %v", got)
	}
	// Discord goes away; the TTL expires so a refresh is attempted and fails.
	fake.fail.Store(true)
	time.Sleep(5 * time.Millisecond)
	got, err := r.AllowedChannels(context.Background(), "U1")
	if err != nil {
		t.Fatalf("must not error while cached data exists: %v", err)
	}
	if !got["C"] {
		t.Fatalf("cached permissions must survive a Discord outage, got %v", got)
	}
	if r.Status().LiveFails == 0 {
		t.Fatal("expected the failed refresh to be recorded")
	}
}

func TestOfflineFallbackToDiskSnapshot(t *testing.T) {
	// No warm cache: live lookup fails immediately, so the on-disk snapshot
	// must be used.
	fake := &fakeAPI{fail: atomic.Bool{}}
	fake.fail.Store(true)
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	path := snapshotFile(t)
	r := New(Config{
		Client:       discord.New("tok", srv.URL),
		SnapshotPath: path,
		GuildID:      "G",
		ChannelTTL:   time.Minute,
		MemberTTL:    time.Minute,
	})
	got, err := r.AllowedChannels(context.Background(), "U1")
	if err != nil {
		t.Fatal(err)
	}
	if !got["C"] {
		t.Fatalf("disk snapshot fallback failed, got %v", got)
	}
}

// TestLiveFailureBackoff ensures a Discord outage does not make every request
// wait out its own timeout: after one failure, live lookups pause briefly and
// requests are answered from cache/snapshot instead.
func TestLiveFailureBackoff(t *testing.T) {
	fake := &fakeAPI{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	path := snapshotFile(t)
	r := New(Config{
		Client:       discord.New("tok", srv.URL),
		SnapshotPath: path,
		GuildID:      "G",
		ChannelTTL:   time.Minute,
		MemberTTL:    time.Minute,
	})
	if got, _ := r.AllowedChannels(context.Background(), "U1"); !got["C"] {
		t.Fatalf("warm-up failed: %v", got)
	}
	// Expire every cache so the next call would attempt a live lookup.
	r.mu.Lock()
	r.channelsAt, r.rolesAt = time.Time{}, time.Time{}
	r.members = map[string]memberEntry{}
	r.mu.Unlock()

	fake.fail.Store(true)
	if got, err := r.AllowedChannels(context.Background(), "U1"); err != nil || !got["C"] {
		t.Fatalf("must answer from cache/snapshot during an outage: got=%v err=%v", got, err)
	}
	fails := r.Status().LiveFails
	if fails == 0 {
		t.Fatal("expected the failed lookup to be recorded")
	}
	// A second call inside the backoff window must not hit the network again.
	if got, _ := r.AllowedChannels(context.Background(), "U1"); !got["C"] {
		t.Fatalf("cached access lost: %v", got)
	}
	if r.Status().LiveFails != fails {
		t.Fatalf("backoff did not suppress a second live attempt: %d -> %d", fails, r.Status().LiveFails)
	}
	// Once the backoff elapses, Discord is retried (and fails again).
	r.mu.Lock()
	r.cfg.RetryTTL = time.Millisecond
	r.liveUntil = time.Time{}
	r.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	if got, _ := r.AllowedChannels(context.Background(), "U1"); !got["C"] {
		t.Fatalf("cached access lost after retry: %v", got)
	}
	if r.Status().LiveFails == fails {
		t.Fatalf("expected a retry once retry_ttl elapsed, LiveFails still %d", fails)
	}
}

func TestUnknownMemberDenied(t *testing.T) {
	fake := &fakeAPI{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	path := snapshotFile(t)
	r := New(Config{
		Client:       discord.New("tok", srv.URL),
		SnapshotPath: path,
		GuildID:      "G",
		ChannelTTL:   time.Minute,
		MemberTTL:    time.Minute,
	})
	got, err := r.AllowedChannels(context.Background(), "U404")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("non-member must be denied, got %v", got)
	}
}

// TestFailClosedUnknownMember guards the fail-closed rule: a user who is
// neither resolvable live nor present in the snapshot must see nothing, even
// when @everyone can read. A known member with no roles still reads via
// @everyone.
func TestFailClosedUnknownMember(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, withMember bool) string {
		path := filepath.Join(dir, name)
		snap := perms.NewSnapshot("G")
		// @everyone: VIEW_CHANNEL (1024) | READ_MESSAGE_HISTORY (65536).
		snap.Roles = []discord.Role{{ID: "G", Permissions: "66560"}}
		snap.SetChannels([]discord.Channel{{ID: "C", Name: "general", Type: 0}})
		if withMember {
			snap.Members["U1"] = perms.MemberRoles{Username: "alice"}
		}
		if err := perms.SaveSnapshot(path, snap); err != nil {
			t.Fatal(err)
		}
		return path
	}

	unknown := New(Config{SnapshotPath: write("unknown.json", false), GuildID: "G"})
	if got, err := unknown.AllowedChannels(context.Background(), "NOBODY"); err != nil || len(got) != 0 {
		t.Fatalf("unknown user must be denied even though @everyone can read, got %v err %v", got, err)
	}

	member := New(Config{SnapshotPath: write("member.json", true), GuildID: "G"})
	if got, err := member.AllowedChannels(context.Background(), "U1"); err != nil || !got["C"] {
		t.Fatalf("known member must read via @everyone, got %v err %v", got, err)
	}
}

func TestSnapshotReloadOnChange(t *testing.T) {
	path := snapshotFile(t)
	r := New(Config{SnapshotPath: path, GuildID: "G"})
	if got, _ := r.AllowedChannels(context.Background(), "U1"); !got["C"] {
		t.Fatalf("initial snapshot not applied: %v", got)
	}
	// Rewrite the snapshot: U1 no longer has R1, so C is unreadable.
	snap := perms.NewSnapshot("G")
	snap.Roles = []discord.Role{{ID: "G", Permissions: "1024"}, {ID: "R1", Permissions: "65536"}}
	snap.SetChannels([]discord.Channel{{ID: "C", Name: "general", Type: 0}})
	snap.Members["U1"] = perms.MemberRoles{Username: "alice"}
	if err := perms.SaveSnapshot(path, snap); err != nil {
		t.Fatal(err)
	}
	// Force a distinct mod time (filesystems may have coarse granularity).
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.AllowedChannels(context.Background(), "U1"); got["C"] {
		t.Fatalf("updated snapshot not reloaded, got %v", got)
	}
}
