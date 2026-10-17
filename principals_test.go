package main

import (
	"bytes"
	"strings"
	"testing"

	"discord-backup/internal/config"
	"discord-backup/internal/discord"
)

// TestMembersToPrincipals covers the contract that makes "principals" safe to
// run repeatedly: existing entries (and their tokens) survive, already
// configured members are not added twice, bots stay out unless asked for, and
// every new member gets its own token.
func TestMembersToPrincipals(t *testing.T) {
	existing := []config.Principal{{Name: "existing", DiscordID: "9", Token: "tok-old"}}
	members := []discord.Member{
		{User: discord.User{ID: "2", Username: "carol"}},
		{User: discord.User{ID: "3", Username: "botty", Bot: true}},
		{User: discord.User{ID: "9", Username: "existing"}},
		{User: discord.User{ID: "1", Username: "alice"}},
	}

	got, added, err := membersToPrincipals(existing, members, principalsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 {
		t.Fatalf("added %d principals (%+v), want 2 (alice, carol)", len(added), added)
	}
	// Sorted by username, existing first: that is the order the file gets.
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	if want := "existing,alice,carol"; strings.Join(names, ",") != want {
		t.Fatalf("names = %s, want %s", strings.Join(names, ","), want)
	}
	if got[0].Token != "tok-old" {
		t.Fatalf("existing token changed: %q", got[0].Token)
	}
	tokens := map[string]string{}
	for _, p := range added {
		if p.Token == "" {
			t.Fatalf("new principal %q has no token", p.Name)
		}
		if _, dup := tokens[p.Token]; dup {
			t.Fatalf("token handed out twice: %q", p.Token)
		}
		tokens[p.Token] = p.Name
	}
	if err := checkPrincipals(got); err != nil {
		t.Fatalf("result must be usable: %v", err)
	}

	// --no-token: objects only, for stdio setups.
	got, added, err = membersToPrincipals(nil, members, principalsOptions{NoToken: true, IncludeBots: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 4 {
		t.Fatalf("added %d, want 4 (bots included)", len(added))
	}
	for _, p := range got {
		if p.Token != "" {
			t.Fatalf("--no-token produced a token: %+v", p)
		}
	}

	// A member without an id is not a principal.
	got, added, err = membersToPrincipals(nil, []discord.Member{{User: discord.User{Username: "ghost"}}}, principalsOptions{})
	if err != nil || len(got) != 0 || len(added) != 0 {
		t.Fatalf("id-less member: %+v %+v %v", got, added, err)
	}
}

// TestUniquePrincipalName: usernames are unique on Discord, but they can clash
// with a name the config already uses - then the id disambiguates.
func TestUniquePrincipalName(t *testing.T) {
	taken := map[string]bool{"alice": true}
	m := discord.Member{User: discord.User{ID: "10112345", Username: "Alice"}}
	if got := uniquePrincipalName(m, taken); got != "Alice-2345" {
		t.Fatalf("name = %q, want Alice-2345", got)
	}
	taken["alice-2345"] = true
	if got := uniquePrincipalName(m, taken); got != "Alice-10112345" {
		t.Fatalf("name = %q, want Alice-10112345", got)
	}
	// No collision: the username is used as-is.
	if got := uniquePrincipalName(discord.Member{User: discord.User{ID: "7", Username: "bob"}}, taken); got != "bob" {
		t.Fatalf("name = %q, want bob", got)
	}
	// No username at all: the id is a better name than nothing.
	if got := uniquePrincipalName(discord.Member{User: discord.User{ID: "7"}}, map[string]bool{}); got != "7" {
		t.Fatalf("name = %q, want 7", got)
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
