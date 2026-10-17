package perms

import (
	"strconv"
	"testing"

	"discord-backup/internal/discord"
)

func bits(v int64) string { return strconv.FormatInt(v, 10) }

func TestParseBits(t *testing.T) {
	if got := ParseBits("1024"); got != ViewChannel {
		t.Fatalf("ParseBits(1024) = %d", got)
	}
	if got := ParseBits(""); got != 0 {
		t.Fatalf("empty = %d, want 0", got)
	}
	if got := ParseBits("not-a-number"); got != 0 {
		t.Fatalf("garbage = %d, want 0 (fail closed)", got)
	}
}

func TestBaseUnion(t *testing.T) {
	roles := []discord.Role{
		{ID: "G", Permissions: bits(ViewChannel)},          // @everyone
		{ID: "R1", Permissions: bits(ReadMessageHistory)},  // member role
		{ID: "R2", Permissions: bits(1024 * 1024)},         // not a member role
		{ID: "R3", Permissions: bits(ViewChannel | 1<<11)}, // member role
	}
	got := Base(roles, []string{"R1", "R3"}, "G")
	want := ViewChannel | ReadMessageHistory | 1<<11
	if got != want {
		t.Fatalf("Base = %d, want %d", got, want)
	}
}

func TestComputeOverwriteTiers(t *testing.T) {
	const guild = "G"
	const user = "U"
	// base: view+read from @everyone
	base := ViewChannel | ReadMessageHistory

	// @everyone deny read -> member cannot read even though base allows.
	ow := []discord.PermissionOverwrite{
		{ID: guild, Type: discord.OverwriteRole, Deny: bits(ReadMessageHistory)},
	}
	if got := Compute(base, ow, user, guild, nil); CanRead(got) {
		t.Fatalf("everyone-deny should block read: %d", got)
	}

	// role allow read beats @everyone deny? No: everyone deny applies first,
	// then role allow re-grants (tier order).
	ow = []discord.PermissionOverwrite{
		{ID: guild, Type: discord.OverwriteRole, Deny: bits(ReadMessageHistory)},
		{ID: "R1", Type: discord.OverwriteRole, Allow: bits(ReadMessageHistory)},
	}
	if got := Compute(base, ow, user, guild, []string{"R1"}); !CanRead(got) {
		t.Fatalf("role allow should restore read: %d", got)
	}
	// ... but only for members of R1.
	if got := Compute(base, ow, user, guild, []string{"R9"}); CanRead(got) {
		t.Fatalf("non-member of R1 must not read: %d", got)
	}

	// member overwrite wins over a role deny.
	ow = []discord.PermissionOverwrite{
		{ID: "R1", Type: discord.OverwriteRole, Deny: bits(ViewChannel | ReadMessageHistory)},
		{ID: user, Type: discord.OverwriteMember, Allow: bits(ViewChannel | ReadMessageHistory)},
	}
	if got := Compute(base, ow, user, guild, []string{"R1"}); !CanRead(got) {
		t.Fatalf("member overwrite should win: %d", got)
	}
	// a member overwrite deny wins over an allow elsewhere.
	ow = []discord.PermissionOverwrite{
		{ID: user, Type: discord.OverwriteMember, Deny: bits(ViewChannel)},
	}
	if got := Compute(base, ow, user, guild, nil); CanRead(got) {
		t.Fatalf("member deny should block: %d", got)
	}
}

func TestComputeAdministratorBypasses(t *testing.T) {
	base := Administrator // admin, no view/read bits
	ow := []discord.PermissionOverwrite{
		{ID: "G", Type: discord.OverwriteRole, Deny: bits(ViewChannel | ReadMessageHistory)},
	}
	got := Compute(base, ow, "U", "G", nil)
	if !CanRead(got) {
		t.Fatalf("administrator must read everything: %d", got)
	}
}

func TestEffectiveOverwritesCategoryThenChannel(t *testing.T) {
	cat := discord.Channel{
		ID: "CAT", Type: discord.ChannelCategory,
		PermissionOverwrites: []discord.PermissionOverwrite{
			{ID: "G", Type: discord.OverwriteRole, Deny: bits(ViewChannel)},
		},
	}
	child := discord.Channel{
		ID: "C", Type: discord.ChannelGuildText, ParentID: ptr("CAT"),
		PermissionOverwrites: []discord.PermissionOverwrite{
			{ID: "G", Type: discord.OverwriteRole, Allow: bits(ViewChannel)},
		},
	}
	byID := map[string]discord.Channel{"CAT": cat, "C": child}
	ow := EffectiveOverwrites(child, byID)
	if len(ow) != 2 {
		t.Fatalf("overwrites = %d, want 2", len(ow))
	}
	// Category deny first, then channel allow -> allow wins.
	base := ViewChannel | ReadMessageHistory
	if got := Compute(base, ow, "U", "G", nil); !CanRead(got) {
		t.Fatalf("channel overwrite should override category: %d", got)
	}
}

func TestCanReadChannelWithRoles(t *testing.T) {
	const guild, user = "G", "U"
	roles := []discord.Role{
		{ID: guild, Permissions: bits(ViewChannel)},       // @everyone: view only
		{ID: "R1", Permissions: bits(ReadMessageHistory)}, // member role adds read
	}
	byID := map[string]discord.Channel{}
	open := discord.Channel{ID: "C1", Type: discord.ChannelGuildText}
	secret := discord.Channel{
		ID: "C2", Type: discord.ChannelGuildText,
		PermissionOverwrites: []discord.PermissionOverwrite{
			{ID: guild, Type: discord.OverwriteRole, Deny: bits(ViewChannel | ReadMessageHistory)},
		},
	}

	if !CanReadChannel(open, byID, roles, []string{"R1"}, user, guild) {
		t.Fatal("member with R1 should read the open channel")
	}
	if CanReadChannel(open, byID, roles, nil, user, guild) {
		t.Fatal("member without R1 lacks read on the open channel")
	}
	if CanReadChannel(secret, byID, roles, []string{"R1"}, user, guild) {
		t.Fatal("everyone-deny must block the secret channel")
	}
}

func ptr(s string) *string { return &s }
