package perms

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"discord-backup/internal/discord"
	"discord-backup/internal/store"
)

// Snapshot is a point-in-time capture of everything needed to compute who
// may read which channel, without talking to Discord:
//
//   - the guild's roles (with permission bitfields), including @everyone
//   - every channel's permission overwrites (categories included)
//   - each member's role IDs
//
// The backup writes one per run to server_<id>/permissions.json, so the
// archive stays self-describing and an offline MCP can still enforce the
// same access rules. Member roles are captured best-effort; guilds too large
// to enumerate fall back to a live lookup.
type Snapshot struct {
	FetchedAt string                  `json:"fetched_at"`
	GuildID   string                  `json:"guild_id"`
	Roles     []discord.Role          `json:"roles"`
	Channels  map[string]ChannelPerms `json:"channels"`
	Members   map[string]MemberRoles  `json:"members"`
}

// ChannelPerms is one channel's entry in a Snapshot.
type ChannelPerms struct {
	Name       string                        `json:"name,omitempty"`
	Type       int                           `json:"type"`
	ParentID   string                        `json:"parent_id,omitempty"`
	Overwrites []discord.PermissionOverwrite `json:"overwrites,omitempty"`
}

// MemberRoles is one member's role IDs (plus a display hint for operators).
type MemberRoles struct {
	Username string   `json:"username,omitempty"`
	Roles    []string `json:"roles"`
}

// NewSnapshot builds an empty snapshot ready to be filled.
func NewSnapshot(guildID string) *Snapshot {
	return &Snapshot{
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		GuildID:   guildID,
		Channels:  map[string]ChannelPerms{},
		Members:   map[string]MemberRoles{},
	}
}

// SetChannels records channel metadata and overwrites. Categories are kept
// too: EffectiveOverwrites needs them for inheritance.
func (s *Snapshot) SetChannels(channels []discord.Channel) {
	if s.Channels == nil {
		s.Channels = map[string]ChannelPerms{}
	}
	for _, ch := range channels {
		cp := ChannelPerms{Name: ch.Name, Type: ch.Type, Overwrites: ch.PermissionOverwrites}
		if ch.ParentID != nil {
			cp.ParentID = *ch.ParentID
		}
		s.Channels[ch.ID] = cp
	}
}

// MemberRolesFor returns the role IDs captured for a member, or nil if the
// member is not in the snapshot (caller may then fall back to a live lookup).
func (s *Snapshot) MemberRolesFor(userID string) ([]string, bool) {
	m, ok := s.Members[userID]
	if !ok {
		return nil, false
	}
	return m.Roles, true
}

// ChannelsList reconstructs the channel list from the snapshot so the same
// computation can run on live data and on snapshotted data.
func (s *Snapshot) ChannelsList() []discord.Channel {
	out := make([]discord.Channel, 0, len(s.Channels))
	for id, cp := range s.Channels {
		ch := discord.Channel{
			ID:                   id,
			Name:                 cp.Name,
			Type:                 cp.Type,
			PermissionOverwrites: cp.Overwrites,
		}
		if cp.ParentID != "" {
			parent := cp.ParentID
			ch.ParentID = &parent
		}
		out = append(out, ch)
	}
	return out
}

// AllowedChannels returns the set of channel IDs the given member may read,
// computed entirely from the snapshot.
func (s *Snapshot) AllowedChannels(userID string) map[string]bool {
	memberRoles, _ := s.MemberRolesFor(userID)
	return ComputeAllowed(s.GuildID, s.Roles, s.ChannelsList(), userID, memberRoles)
}

// ComputeAllowed returns the set of readable channel IDs for a member with
// the given role IDs, from raw guild roles and channels. It is the single
// implementation shared by the offline snapshot path and the live path.
func ComputeAllowed(guildID string, roles []discord.Role, channels []discord.Channel, userID string, userRoles []string) map[string]bool {
	byID := make(map[string]discord.Channel, len(channels))
	for _, ch := range channels {
		byID[ch.ID] = ch
	}
	base := Base(roles, userRoles, guildID)
	out := make(map[string]bool, len(channels))
	for _, ch := range channels {
		if CanRead(Compute(base, EffectiveOverwrites(ch, byID), userID, guildID, userRoles)) {
			out[ch.ID] = true
		}
	}
	return out
}

// SaveSnapshot writes the snapshot atomically (temp file + rename).
func SaveSnapshot(path string, s *Snapshot) error {
	if err := store.WriteJSON(path, s); err != nil {
		return fmt.Errorf("write permission snapshot: %w", err)
	}
	return nil
}

// LoadSnapshot reads a snapshot from disk. A missing file yields (nil, nil)
// so callers can distinguish "no snapshot yet" from a real error.
func LoadSnapshot(path string) (*Snapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse permission snapshot: %w", err)
	}
	return &s, nil
}
