package main

import (
	"bytes"
	"strings"
	"testing"

	"discord-backup/internal/config"
	"discord-backup/internal/discord"
)

func nick(s string) *string { return &s }

func names(principals []config.Principal) string {
	out := make([]string, 0, len(principals))
	for _, p := range principals {
		out = append(out, p.Name)
	}
	return strings.Join(out, ",")
}

// TestMembersToPrincipals covers the contract that makes "principals" safe to
// run repeatedly: existing entries keep their token, already configured members
// are not added twice, bots stay out unless asked for, and every new member
// gets its own token.
func TestMembersToPrincipals(t *testing.T) {
	existing := []config.Principal{{Name: "existing", DiscordID: "9", Token: "tok-old"}}
	members := []discord.Member{
		{User: discord.User{ID: "2", Username: "carol"}},
		{User: discord.User{ID: "3", Username: "botty", Bot: true}},
		{User: discord.User{ID: "9", Username: "existing", GlobalName: "Existing Person"}},
		{User: discord.User{ID: "1", Username: "alice"}},
	}

	res, err := membersToPrincipals(existing, members, principalsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 2 {
		t.Fatalf("added %d principals (%+v), want 2 (alice, carol)", len(res.Added), res.Added)
	}
	// Existing first (in config order), new ones alphabetically after them.
	if got, want := names(res.All), "Existing Person,alice,carol"; got != want {
		t.Fatalf("names = %s, want %s", got, want)
	}
	if res.All[0].Token != "tok-old" {
		t.Fatalf("existing token changed: %q", res.All[0].Token)
	}
	if len(res.Renamed) != 1 || res.Renamed[0].Name != "Existing Person" {
		t.Fatalf("renamed = %+v, want the display name", res.Renamed)
	}
	tokens := map[string]string{}
	for _, p := range res.Added {
		if p.Token == "" {
			t.Fatalf("new principal %q has no token", p.Name)
		}
		if _, dup := tokens[p.Token]; dup {
			t.Fatalf("token handed out twice: %q", p.Token)
		}
		tokens[p.Token] = p.Name
	}
	if err := checkPrincipals(res.All); err != nil {
		t.Fatalf("result must be usable: %v", err)
	}

	// --no-token: objects only, for stdio setups.
	res, err = membersToPrincipals(nil, members, principalsOptions{NoToken: true, IncludeBots: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 4 {
		t.Fatalf("added %d, want 4 (bots included)", len(res.Added))
	}
	for _, p := range res.All {
		if p.Token != "" {
			t.Fatalf("--no-token produced a token: %+v", p)
		}
	}

	// A member without an id is not a principal.
	res, err = membersToPrincipals(nil, []discord.Member{{User: discord.User{Username: "ghost"}}}, principalsOptions{})
	if err != nil || len(res.All) != 0 || len(res.Added) != 0 {
		t.Fatalf("id-less member: %+v %v", res, err)
	}
}

// TestMembersToPrincipalsNames: the label is what the server calls the person,
// not the login handle, and collisions stay readable.
func TestMembersToPrincipalsNames(t *testing.T) {
	members := []discord.Member{
		{ // nickname wins over display name and handle, whitespace is cleaned
			User: discord.User{ID: "1", Username: "aduffeck_63702", GlobalName: "aduffeck"},
			Nick: nick("  Alexander \n Duffeck "),
		},
		{ // the same full name a second time: the handle asks which one
			User: discord.User{ID: "2", Username: "aduffeck2", GlobalName: "Alexander Duffeck"},
		},
		{User: discord.User{ID: "3", Username: "carol"}},                // handle only
		{User: discord.User{ID: "4"}},                                   // nothing at all
		{User: discord.User{ID: "5", Username: "x"}, Nick: nick("   ")}, // blank nick falls through
	}

	res, err := membersToPrincipals(nil, members, principalsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"1": "Alexander Duffeck",
		"2": "Alexander Duffeck (aduffeck2)",
		"3": "carol",
		"4": "4",
		"5": "x",
	}
	byID := map[string]string{}
	for _, p := range res.All {
		byID[p.DiscordID] = p.Name
	}
	for id, name := range want {
		if byID[id] != name {
			t.Errorf("member %s named %q, want %q", id, byID[id], name)
		}
	}
	if err := checkPrincipals(res.All); err != nil {
		t.Fatalf("names must be unique: %v", err)
	}
}

// TestMembersToPrincipalsKeepNames: --keep-names is the escape hatch for
// someone who renamed a principal on purpose.
func TestMembersToPrincipalsKeepNames(t *testing.T) {
	existing := []config.Principal{{Name: "le chef", DiscordID: "1", Token: "tok"}}
	members := []discord.Member{{User: discord.User{ID: "1", Username: "aduffeck_63702", GlobalName: "Alexander Duffeck"}}}

	res, err := membersToPrincipals(existing, members, principalsOptions{KeepNames: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(res.All); got != "le chef" {
		t.Fatalf("names = %s, want the configured one", got)
	}
	if len(res.Renamed) != 0 {
		t.Fatalf("nothing may be renamed: %+v", res.Renamed)
	}
}

// TestMembersToPrincipalsRenamesAgainstAliases: a name that is taken by an
// alias must not be handed out twice.
func TestMembersToPrincipalsRenamesAgainstAliases(t *testing.T) {
	existing := []config.Principal{{Name: "boss", DiscordID: "9", Aliases: []string{"Alexander Duffeck"}}}
	members := []discord.Member{{User: discord.User{ID: "1", Username: "aduffeck", GlobalName: "Alexander Duffeck"}}}

	res, err := membersToPrincipals(existing, members, principalsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(res.All); got != "boss,Alexander Duffeck (aduffeck)" {
		t.Fatalf("names = %s, want the alias to stay unique", got)
	}
}

// TestUniqueName: an existing name is disambiguated with the handle, then the
// ID - and never with an opaque digit tail when the handle says more.
func TestUniqueName(t *testing.T) {
	m := discord.Member{User: discord.User{ID: "10112345", Username: "alex"}, Nick: nick("Alex")}
	// The handle equals the name (only the case differs), so the ID has to
	// disambiguate.
	if got := uniqueName(m, map[string]bool{"alex": true}); got != "Alex (10112345)" {
		t.Fatalf("name = %q, want Alex (10112345)", got)
	}
	if got := uniqueName(m, map[string]bool{"alex": true, "alex (10112345)": true}); got != "Alex (10112345 2)" {
		t.Fatalf("name = %q, want Alex (10112345 2)", got)
	}
	// A handle that differs from the name is the friendlier disambiguator.
	other := discord.Member{User: discord.User{ID: "7", Username: "aduffeck", GlobalName: "Alex"}}
	if got := uniqueName(other, map[string]bool{"alex": true}); got != "Alex (aduffeck)" {
		t.Fatalf("name = %q, want Alex (aduffeck)", got)
	}
	// No handle to speak of: the ID beats nothing.
	plain := discord.Member{User: discord.User{ID: "7", Username: "bob"}}
	if got := uniqueName(plain, map[string]bool{"bob": true}); got != "bob (7)" {
		t.Fatalf("name = %q, want bob (7)", got)
	}
}

func TestNewPrincipalToken(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		tok, err := newPrincipalToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != 43 { // 32 bytes base64url, unpadded
			t.Fatalf("token %q has length %d, want 43", tok, len(tok))
		}
		if strings.ContainsAny(tok, "+/=") {
			t.Fatalf("token %q is not URL-safe", tok)
		}
		if seen[tok] {
			t.Fatalf("token repeated: %q", tok)
		}
		seen[tok] = true
	}
}

// TestCheckPrincipals: the guard that runs before anything is written.
func TestCheckPrincipals(t *testing.T) {
	ok := []config.Principal{{Name: "alice", DiscordID: "1", Token: "a"}, {Name: "bob", DiscordID: "2"}}
	if err := checkPrincipals(ok); err != nil {
		t.Fatalf("valid list rejected: %v", err)
	}
	for _, bad := range [][]config.Principal{
		{{Name: "alice"}},
		{{Name: "", DiscordID: "1"}},
		{{Name: "alice", DiscordID: "1"}, {Name: "ALICE", DiscordID: "2"}},
		{{Name: "alice", DiscordID: "1"}, {Name: "bob", DiscordID: "1"}},
		{{Name: "alice", DiscordID: "1", Token: "t"}, {Name: "bob", DiscordID: "2", Token: "t"}},
	} {
		if err := checkPrincipals(bad); err == nil {
			t.Fatalf("invalid list accepted: %+v", bad)
		}
	}
}

// TestMatchChannels: names with and without "#", and IDs; a value that matches
// nothing is an error, because it would silently keep everybody.
func TestMatchChannels(t *testing.T) {
	all := []discord.Channel{
		{ID: "C1", Name: "support"},
		{ID: "C2", Name: "Support"}, // a duplicate name is allowed by Discord
		{ID: "C3", Name: "general"},
	}
	got, err := matchChannels(all, []string{"#support", "C3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("matched %d channels (%+v), want 3", len(got), got)
	}
	if _, err := matchChannels(all, []string{"nope"}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("unknown channel: %v", err)
	}
}

// TestPrintPrincipalsFragment pins the indentation: the block has to slot into
// the "mcp" object of the config as-is.
func TestPrintPrincipalsFragment(t *testing.T) {
	var buf bytes.Buffer
	if err := printPrincipalsFragment(&buf, []config.Principal{{Name: "martin", DiscordID: "U1", Token: "tok"}}); err != nil {
		t.Fatal(err)
	}
	want := `    "principals": [
      {
        "name": "martin",
        "discord_id": "U1",
        "token": "tok"
      }
    ]
`
	if buf.String() != want {
		t.Fatalf("fragment:\n%s\nwant:\n%s", buf.String(), want)
	}
	// An empty list still has to be valid JSON for the config.
	buf.Reset()
	if err := printPrincipalsFragment(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"principals": []`) {
		t.Fatalf("empty list: %q", buf.String())
	}
}
