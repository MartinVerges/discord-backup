// Package perms computes Discord channel permissions for a guild member.
//
// Discord exposes no "can user X view channel Y" endpoint, so the filter
// that guards user-scoped access has to reproduce Discord's permission
// algorithm:
//
//	base = @everyone.permissions | OR(role.permissions for role in member roles)
//	if base has ADMINISTRATOR: the member has every permission
//	otherwise apply overwrites in tiers: @everyone, then roles, then member
//	    perms = (perms &^ deny) | allow    at each tier
//
// Overwrites on a category channel apply to its children first, then the
// channel's own overwrites (which is why a channel "synced" to its category
// behaves the same either way: identical overwrites are idempotent here).
//
// Reading a channel's message history requires VIEW_CHANNEL and
// READ_MESSAGE_HISTORY.
//
// The functions are pure: fetching roles, channels and members is the
// caller's job, which keeps the algorithm trivially testable.
package perms

import (
	"strconv"

	"discord-backup/internal/discord"
)

// Permission bits used by the filter.
const (
	Administrator      int64 = 1 << 3
	ViewChannel        int64 = 1 << 10
	ReadMessageHistory int64 = 1 << 16

	// All is returned when a member has ADMINISTRATOR (every permission).
	All int64 = ^int64(0)
)

// ParseBits converts a decimal permission bitfield string to an int64.
// Empty or malformed values yield 0 (fail closed).
func ParseBits(s string) int64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return int64(v)
}

// Base returns the union of the @everyone role (ID == guildID) and every
// role listed in memberRoles.
func Base(roles []discord.Role, memberRoles []string, guildID string) int64 {
	member := make(map[string]struct{}, len(memberRoles))
	for _, r := range memberRoles {
		member[r] = struct{}{}
	}
	var perms int64
	for _, r := range roles {
		if r.ID == guildID {
			perms |= ParseBits(r.Permissions)
			continue
		}
		if _, ok := member[r.ID]; ok {
			perms |= ParseBits(r.Permissions)
		}
	}
	return perms
}

// Compute returns the effective permissions of a member in one channel.
// overwrites must already include the category's overwrites (if any) ahead
// of the channel's own; see EffectiveOverwrites.
func Compute(base int64, overwrites []discord.PermissionOverwrite, userID, guildID string, memberRoles []string) int64 {
	if base&Administrator != 0 {
		return All
	}
	member := make(map[string]struct{}, len(memberRoles))
	for _, r := range memberRoles {
		member[r] = struct{}{}
	}

	perms := base
	// Tier 1: the @everyone overwrite (type role, id == guildID).
	for _, o := range overwrites {
		if o.Type == discord.OverwriteRole && o.ID == guildID {
			perms = (perms &^ ParseBits(o.Deny)) | ParseBits(o.Allow)
		}
	}
	// Tier 2: union of the member's role overwrites.
	var allow, deny int64
	for _, o := range overwrites {
		if o.Type != discord.OverwriteRole || o.ID == guildID {
			continue
		}
		if _, ok := member[o.ID]; ok {
			allow |= ParseBits(o.Allow)
			deny |= ParseBits(o.Deny)
		}
	}
	perms = (perms &^ deny) | allow
	// Tier 3: the member's own overwrite (wins over everything).
	for _, o := range overwrites {
		if o.Type == discord.OverwriteMember && o.ID == userID {
			perms = (perms &^ ParseBits(o.Deny)) | ParseBits(o.Allow)
		}
	}
	return perms
}

// EffectiveOverwrites returns the overwrite list for a channel: its
// category's overwrites first, then the channel's own. byID must contain the
// guild's channels so the parent (category) can be resolved.
func EffectiveOverwrites(ch discord.Channel, byID map[string]discord.Channel) []discord.PermissionOverwrite {
	var out []discord.PermissionOverwrite
	if ch.ParentID != nil {
		if cat, ok := byID[*ch.ParentID]; ok && cat.Type == discord.ChannelCategory {
			out = append(out, cat.PermissionOverwrites...)
		}
	}
	out = append(out, ch.PermissionOverwrites...)
	return out
}

// CanRead reports whether the permission set allows reading message history.
func CanRead(perms int64) bool {
	return perms&ViewChannel != 0 && perms&ReadMessageHistory != 0
}

// CanReadChannel is the convenience used by the access filter: it computes
// the member's permissions for ch (category + channel overwrites) and
// reports whether history may be read.
func CanReadChannel(ch discord.Channel, byID map[string]discord.Channel, roles []discord.Role, memberRoles []string, userID, guildID string) bool {
	base := Base(roles, memberRoles, guildID)
	perms := Compute(base, EffectiveOverwrites(ch, byID), userID, guildID, memberRoles)
	return CanRead(perms)
}
