// Package authz answers "which archived channels may this user read?".
//
// It wraps the pure permission math of internal/perms with a small cache:
//
//   - guild channel overwrites and roles are cached for ChannelTTL
//   - one member's role IDs are cached for MemberTTL
//
// Every lookup prefers fresh cached data, then a live API call, then the
// cached (stale) data and finally the permission snapshot written by the
// backup at archive time. That last fallback is what keeps the tool usable
// while Discord is unreachable: the archive carries its own permission
// snapshot, refreshed on every backup run.
//
// Fail-closed by construction: if no source can prove access, the caller
// gets an empty set.
package authz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"discord-backup/internal/discord"
	"discord-backup/internal/perms"
)

// Config wires a Resolver to a Discord client and an archive snapshot.
type Config struct {
	// Client may be nil for a strictly offline resolver (snapshot only).
	Client *discord.Client
	// SnapshotPath is server_<id>/permissions.json. May be empty to disable
	// the disk fallback.
	SnapshotPath string
	// GuildID is needed to resolve the @everyone overwrite. Required.
	GuildID string
	// ChannelTTL is how long channel overwrites and roles stay fresh.
	// Zero disables the live lookup entirely (snapshot only).
	ChannelTTL time.Duration
	// MemberTTL is how long one member's role IDs stay fresh.
	MemberTTL time.Duration
	// RetryTTL is how long live lookups pause after a failure before Discord
	// is retried. Defaults to 5m.
	RetryTTL time.Duration
	// Timeout bounds each live lookup so a Discord outage degrades to the
	// cache/snapshot quickly instead of stalling a query. Defaults to 5s.
	Timeout time.Duration
	Log     *slog.Logger
}

type memberEntry struct {
	roles  []string
	at     time.Time
	gen    int  // snapshot generation this came from; -1 for a live lookup
	denied bool // Discord explicitly reported "not a guild member"
}

// Resolver caches permission inputs and computes per-user channel access.
type Resolver struct {
	cfg Config
	log *slog.Logger

	mu         sync.Mutex
	channels   []discord.Channel
	channelsAt time.Time
	roles      []discord.Role
	rolesAt    time.Time
	members    map[string]memberEntry

	disk      *perms.Snapshot
	diskMod   time.Time
	snapGen   int
	liveFails int
	liveUntil time.Time // suppress live lookups until this time after a failure
}

// New returns a Resolver. It does not talk to Discord yet.
func New(cfg Config) *Resolver {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Resolver{cfg: cfg, log: log, members: map[string]memberEntry{}}
}

// AllowedChannels returns the set of channel IDs the user may read.
func (r *Resolver) AllowedChannels(ctx context.Context, userID string) (map[string]bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	guildID, roles, channels, err := r.inputsLocked(ctx)
	if err != nil {
		return nil, err
	}
	userRoles, known := r.memberRolesLocked(ctx, userID)
	if !known {
		// Fail closed: a user we cannot prove is a guild member gets nothing,
		// even if @everyone would otherwise grant read access.
		r.log.Info("no membership evidence, denying all channels", "user", userID)
		return map[string]bool{}, nil
	}
	return perms.ComputeAllowed(guildID, roles, channels, userID, userRoles), nil
}

// AllowedChannelsForRoles is AllowedChannels for a caller that already knows
// the member's role IDs (the "principals" command reads them straight from the
// member list and must not pay one API call per member). It deliberately skips
// the membership check: an empty role list means a member without roles, which
// is exactly what @everyone-only access looks like. The caller has to have
// established membership itself.
func (r *Resolver) AllowedChannelsForRoles(ctx context.Context, userID string, roleIDs []string) (map[string]bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	guildID, roles, channels, err := r.inputsLocked(ctx)
	if err != nil {
		return nil, err
	}
	return perms.ComputeAllowed(guildID, roles, channels, userID, roleIDs), nil
}

// inputsLocked returns the guild ID, roles and channels a permission
// computation needs: live when possible, otherwise the archive snapshot as a
// unit, so channels and roles always come from the same point in time.
func (r *Resolver) inputsLocked(ctx context.Context) (string, []discord.Role, []discord.Channel, error) {
	guildID := r.cfg.GuildID
	channels := r.channelsLocked(ctx)
	// Only bother with roles when we have channels to apply them to: if the
	// channel lookup failed there is nothing to compute, and the snapshot is
	// used wholesale below anyway.
	var roles []discord.Role
	if channels != nil {
		roles = r.rolesLocked(ctx)
	}

	// If live data is unavailable, use the archive snapshot wholesale so
	// channels and roles always come from the same point in time.
	if channels == nil || roles == nil {
		if s := r.diskLocked(); s != nil {
			channels, roles = s.ChannelsList(), s.Roles
			if guildID == "" {
				guildID = s.GuildID
			}
		}
	}
	if channels == nil || roles == nil {
		return "", nil, nil, fmt.Errorf("no permission data: Discord unreachable and no usable snapshot at %q", r.cfg.SnapshotPath)
	}
	return guildID, roles, channels, nil
}

// live reports whether live API lookups are configured at all.
func (r *Resolver) live() bool { return r.cfg.Client != nil }

// callCtx bounds one live lookup so an outage cannot stall a query.
func (r *Resolver) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := r.cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return context.WithTimeout(ctx, timeout)
}

// liveBlocked reports whether a recent failure should suppress another live
// lookup for a short while. Without this, every request during a Discord
// outage would wait out its own timeout (serialised by the resolver lock)
// instead of answering straight from the cache/snapshot.
func (r *Resolver) liveBlocked() bool { return time.Now().Before(r.liveUntil) }

// noteLiveFailure counts a failed live lookup and opens a short backoff.
func (r *Resolver) noteLiveFailure() {
	r.liveFails++
	if b := r.failBackoff(); b > 0 {
		r.liveUntil = time.Now().Add(b)
	}
}

// noteLiveSuccess clears the backoff after a successful live lookup.
func (r *Resolver) noteLiveSuccess() { r.liveUntil = time.Time{} }

// failBackoff is how long live lookups stay suppressed after a failure, so a
// Discord outage is retried at most every RetryTTL instead of on every call.
func (r *Resolver) failBackoff() time.Duration {
	if r.cfg.RetryTTL > 0 {
		return r.cfg.RetryTTL
	}
	return 5 * time.Minute
}

// channelsLocked returns live channel permissions when possible, else nil so
// the caller can fall back to the snapshot.
func (r *Resolver) channelsLocked(ctx context.Context) []discord.Channel {
	if !r.live() || r.cfg.ChannelTTL <= 0 {
		return nil
	}
	if r.channels != nil && time.Since(r.channelsAt) < r.cfg.ChannelTTL {
		return r.channels
	}
	if r.liveBlocked() {
		return r.channels // recent failure: serve cache/snapshot
	}
	cctx, cancel := r.callCtx(ctx)
	defer cancel()
	list, err := r.cfg.Client.ListChannels(cctx, r.cfg.GuildID)
	if err != nil {
		r.noteLiveFailure()
		r.log.Warn("live channel permissions unavailable, using cache/snapshot", "err", err)
		return r.channels // may be nil
	}
	r.channels, r.channelsAt = list, time.Now()
	r.noteLiveSuccess()
	return list
}

// rolesLocked returns live guild roles when possible, else nil.
func (r *Resolver) rolesLocked(ctx context.Context) []discord.Role {
	if !r.live() || r.cfg.ChannelTTL <= 0 {
		return nil
	}
	if r.roles != nil && time.Since(r.rolesAt) < r.cfg.ChannelTTL {
		return r.roles
	}
	if r.liveBlocked() {
		return r.roles
	}
	cctx, cancel := r.callCtx(ctx)
	defer cancel()
	list, err := r.cfg.Client.Roles(cctx, r.cfg.GuildID)
	if err != nil {
		r.noteLiveFailure()
		r.log.Warn("live roles unavailable, using cache/snapshot", "err", err)
		return r.roles // may be nil
	}
	r.roles, r.rolesAt = list, time.Now()
	r.noteLiveSuccess()
	return list
}

// memberRolesLocked returns the user's role IDs and whether membership could
// be established at all. An unknown user (not in the guild and not in the
// snapshot) is reported as not known, which denies everything for that user
// even when @everyone grants read access to guild members.
func (r *Resolver) memberRolesLocked(ctx context.Context, userID string) (roles []string, known bool) {
	cached, hasCached := r.members[userID]
	if hasCached {
		if cached.denied && r.ttlFresh(cached.at, r.cfg.MemberTTL) {
			return nil, false
		}
		if !cached.denied && cached.gen < 0 && r.ttlFresh(cached.at, r.cfg.MemberTTL) {
			return cached.roles, true
		}
	}
	if r.live() && r.cfg.MemberTTL > 0 && !r.liveBlocked() {
		cctx, cancel := r.callCtx(ctx)
		m, err := r.cfg.Client.Member(cctx, r.cfg.GuildID, userID)
		cancel()
		if err == nil {
			r.members[userID] = memberEntry{roles: m.Roles, at: time.Now(), gen: -1}
			r.noteLiveSuccess()
			return m.Roles, true
		}
		var nf *discord.NotFoundError
		if errors.As(err, &nf) {
			// Discord says the user is not a member: record a short-lived
			// denial so the answer is stable within MemberTTL.
			r.log.Info("user is not a guild member, denying", "user", userID)
			r.members[userID] = memberEntry{at: time.Now(), gen: -1, denied: true}
			return nil, false
		}
		r.noteLiveFailure()
		r.log.Warn("live member roles unavailable, using cache/snapshot", "user", userID, "err", err)
	}
	// Reuse a live entry (stale but the freshest we have).
	if hasCached && !cached.denied && cached.gen < 0 {
		return cached.roles, true
	}
	// Fall back to the snapshot's captured roles for this member.
	if s := r.diskLocked(); s != nil {
		if roles, ok := s.MemberRolesFor(userID); ok {
			r.members[userID] = memberEntry{roles: roles, at: time.Now(), gen: r.snapGen}
			return roles, true
		}
	}
	return nil, false
}

// ttlFresh reports whether at is within ttl (a non-positive ttl disables it).
func (r *Resolver) ttlFresh(at time.Time, ttl time.Duration) bool {
	return ttl > 0 && time.Since(at) < ttl
}

// diskLocked loads the archive snapshot, reloading it when the backup
// rewrites the file. The last successfully loaded snapshot is kept when the
// file is temporarily unreadable.
func (r *Resolver) diskLocked() *perms.Snapshot {
	if r.cfg.SnapshotPath == "" {
		return r.disk
	}
	info, err := os.Stat(r.cfg.SnapshotPath)
	if err != nil {
		return r.disk
	}
	if r.disk != nil && info.ModTime().Equal(r.diskMod) {
		return r.disk
	}
	s, err := perms.LoadSnapshot(r.cfg.SnapshotPath)
	if err != nil {
		r.log.Warn("permission snapshot unreadable", "path", r.cfg.SnapshotPath, "err", err)
		return r.disk
	}
	if s != nil {
		r.disk, r.diskMod = s, info.ModTime()
		// A new snapshot invalidates roles captured from an older one.
		r.snapGen++
		for id, e := range r.members {
			if e.gen >= 0 && e.gen != r.snapGen {
				delete(r.members, id)
			}
		}
	}
	return r.disk
}

// Status describes the resolver's cache state (for the whoami tool).
type Status struct {
	HasSnapshot     bool
	SnapshotFetched string
	LiveChannels    bool
	LiveFails       int
}

// Status reports what the resolver currently uses.
func (r *Resolver) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.diskLocked()
	st := Status{HasSnapshot: s != nil, LiveChannels: r.channels != nil, LiveFails: r.liveFails}
	if s != nil {
		st.SnapshotFetched = s.FetchedAt
	}
	return st
}
