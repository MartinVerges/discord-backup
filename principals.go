package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"discord-backup/internal/authz"
	"discord-backup/internal/config"
	"discord-backup/internal/discord"
	"discord-backup/internal/perms"
)

// apiBase is empty in production (the real Discord API); the tests point it at
// a stub server. It is deliberately not configurable: no config key, no flag.
var apiBase string

// nameList is a repeatable flag that also accepts comma-separated values, so
// "--from-channel a --from-channel b" and "--from-channel a,b" are the same.
type nameList []string

func (n *nameList) String() string { return strings.Join(*n, ",") }

func (n *nameList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*n = append(*n, part)
		}
	}
	return nil
}

// runPrincipals lists the guild's members and turns them into
// "mcp.principals" entries: names and IDs come from Discord, so nobody has to
// copy user IDs by hand. By default the objects are printed for you to paste
// into the config; --write merges them into the file directly.
func runPrincipals(args []string) int {
	fs := flag.NewFlagSet("principals", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", defaultConfigPath, "configuration file (its token and guild_id are used)")
	var fromChannel nameList
	fs.Var(&fromChannel, "from-channel", "only members who may read this channel (name, #name or ID, repeatable)")
	includeBots := fs.Bool("include-bots", false, "also emit bot accounts (those are clients, not people)")
	noToken := fs.Bool("no-token", false, `leave out "token": stdio needs none, HTTP requires one per principal`)
	keepNames := fs.Bool("keep-names", false, "leave the names of already configured principals alone")
	write := fs.Bool("write", false, "merge the result into the config file (atomic, mode 0600)")
	timeout := fs.Duration("timeout", 5*time.Minute, "overall timeout for listing the members")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `discord-backup principals: build "mcp.principals" from the guild member list.

Usage:
  discord-backup principals --config %s
  discord-backup principals --config %s --from-channel support --write

Reads guild_id and token from the same configuration file the backup and the
MCP server use and asks Discord for the member list (this needs the privileged
"Server Members Intent" for the bot).

Names are the names people use on the server: the server nickname if there is
one, otherwise the display name, otherwise the handle. Existing entries keep
their token and are renamed to the current name (--keep-names leaves them
alone), members that are already configured are never added twice, and every
new principal gets its own token - so running this again changes nothing.

Options:
`, defaultConfigPath, defaultConfigPath)
		printFlags(fs)
		fmt.Fprintf(os.Stderr, `
Examples:
  # see who Discord knows, then paste the block into the config
  discord-backup principals --config %s

  # only the people who may read #support-tickets
  discord-backup principals --config %s --from-channel support

  # or let it write the file (the directory must be writable)
  discord-backup principals --config %s --write
`, defaultConfigPath, defaultConfigPath, defaultConfigPath)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "principals: --config is required")
		return 2
	}

	opts, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "principals: %v\n", err)
		return 1
	}
	if strings.TrimSpace(opts.Token) == "" {
		fmt.Fprintf(os.Stderr, "principals: no bot token in %s: set \"token\"\n", *configPath)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	api := discord.New(opts.Token, apiBase)
	guild, err := api.Guild(ctx, opts.GuildID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "principals: %v\n", err)
		return 1
	}
	members, err := listAllMembers(ctx, api, opts.GuildID)
	if err != nil {
		var forbidden *discord.ForbiddenError
		if errors.As(err, &forbidden) {
			fmt.Fprint(os.Stderr, "principals: Discord refused the member list (403). Enable the privileged \"Server Members Intent\" for the bot: Developer Portal -> your app -> Bot -> Privileged Gateway Intents.\n")
		}
		fmt.Fprintf(os.Stderr, "principals: %v\n", err)
		return 1
	}

	total, humans := len(members), countHumans(members)
	access := ""
	if len(fromChannel) > 0 {
		targets, err := resolveChannels(ctx, api, opts, fromChannel)
		if err != nil {
			fmt.Fprintf(os.Stderr, "principals: %v\n", err)
			return 1
		}
		kept, err := filterByChannel(ctx, api, opts, targets, members)
		if err != nil {
			fmt.Fprintf(os.Stderr, "principals: %v\n", err)
			return 1
		}
		if len(kept) == 0 {
			fmt.Fprintf(os.Stderr, "principals: nobody may read %s - check the channel's permissions (VIEW_CHANNEL and READ_MESSAGE_HISTORY)\n", channelNames(targets))
		}
		access = fmt.Sprintf(", %d may read %s (%d excluded)", len(kept), channelNames(targets), total-len(kept))
		members = kept
	}

	res, err := membersToPrincipals(opts.MCP.Principals, members, principalsOptions{
		IncludeBots: *includeBots,
		NoToken:     *noToken,
		KeepNames:   *keepNames,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "principals: %v\n", err)
		return 1
	}
	if err := checkPrincipals(res.All); err != nil {
		fmt.Fprintf(os.Stderr, "principals: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "%s: %d members (%d human)%s\n", guild.Name, total, humans, access)
	fmt.Fprintf(os.Stderr, "%d already configured%s, %d new%s\n",
		len(opts.MCP.Principals), renamedNote(len(res.Renamed)), len(res.Added), botHint(*includeBots))

	if *write {
		inPlace, err := config.SavePrincipals(*configPath, res.All)
		if err != nil {
			fmt.Fprintf(os.Stderr, "principals: %v\n", err)
			return 1
		}
		note := ""
		if inPlace {
			note = " in place (a mounted config cannot be replaced, so the file was rewritten)"
		}
		fmt.Fprintf(os.Stderr, "wrote %d principals to %s%s (restart the server: docker compose up -d)\n", len(res.All), *configPath, note)
		printPrincipalTable(os.Stdout, res.All)
		return 0
	}

	fmt.Fprintf(os.Stderr, "paste this inside the \"mcp\" object of %s (drop the entries that should not connect)\n", *configPath)
	if err := printPrincipalsFragment(os.Stdout, res.All); err != nil {
		fmt.Fprintf(os.Stderr, "principals: %v\n", err)
		return 1
	}
	return 0
}

// listAllMembers walks the guild member list page by page. The cursor follows
// the page (like everywhere else in this tool) so a member that gets filtered
// out cannot stall the loop, and a page that fails to advance ends it.
func listAllMembers(ctx context.Context, api *discord.Client, guildID string) ([]discord.Member, error) {
	var all []discord.Member
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := api.ListMembers(ctx, guildID, after, 1000)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return all, nil
		}
		all = append(all, page...)
		next := page[len(page)-1].User.ID
		if len(page) < 1000 || next == after {
			return all, nil
		}
		after = next
	}
}

// --- channel access filter -------------------------------------------------

// resolveChannels turns --from-channel values into channels, preferring live
// data and falling back to the archive snapshot when Discord is unreachable
// (the same snapshot the MCP server uses as its offline fallback).
func resolveChannels(ctx context.Context, api *discord.Client, opts *config.Options, want []string) ([]discord.Channel, error) {
	all, err := api.ListChannels(ctx, opts.GuildID)
	if err != nil {
		snap, serr := perms.LoadSnapshot(snapshotPath(opts))
		if serr != nil || snap == nil {
			return nil, fmt.Errorf("cannot read the channel list: %w", err)
		}
		all = snap.ChannelsList()
	}
	return matchChannels(all, want)
}

// matchChannels matches names (with or without "#") and IDs, case-insensitively
// on the name - the same rule the backup's channel filter uses. Every value has
// to match something: a typo would otherwise silently keep everybody.
func matchChannels(all []discord.Channel, want []string) ([]discord.Channel, error) {
	var out []discord.Channel
	var missing []string
	for _, w := range want {
		name := strings.TrimPrefix(strings.TrimSpace(w), "#")
		found := false
		for _, ch := range all {
			if ch.ID != w && !strings.EqualFold(ch.Name, name) {
				continue
			}
			found = true
			if !hasChannel(out, ch.ID) {
				out = append(out, ch)
			}
		}
		if !found {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("no channel named %s (a thread inherits from its parent channel, pass that)",
			strings.Join(missing, ", "))
	}
	return out, nil
}

func hasChannel(channels []discord.Channel, id string) bool {
	for _, ch := range channels {
		if ch.ID == id {
			return true
		}
	}
	return false
}

func channelNames(channels []discord.Channel) string {
	names := make([]string, 0, len(channels))
	for _, ch := range channels {
		names = append(names, "#"+ch.Name)
	}
	return strings.Join(names, ", ")
}

// filterByChannel keeps the members who may read any of the given channels.
// The rule is the archive's and the MCP server's rule, because it is the same
// permission math (internal/perms): VIEW_CHANNEL *and* READ_MESSAGE_HISTORY on
// that channel, with category overwrites, channel overwrites, roles - and
// administrator bypassing all of it.
func filterByChannel(ctx context.Context, api *discord.Client, opts *config.Options, targets []discord.Channel, members []discord.Member) ([]discord.Member, error) {
	res := authz.New(authz.Config{
		Client:       api,
		SnapshotPath: snapshotPath(opts),
		GuildID:      opts.GuildID,
		ChannelTTL:   5 * time.Minute,
		Timeout:      30 * time.Second,
		RetryTTL:     5 * time.Minute,
		// Warnings about falling back to the snapshot are useful, the
		// per-call chatter is not.
		Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	var keep []discord.Member
	for i, m := range members {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		allowed, err := res.AllowedChannelsForRoles(ctx, m.User.ID, m.Roles)
		if err != nil {
			return nil, err
		}
		if anyAllowed(allowed, targets) {
			keep = append(keep, m)
		}
		if (i+1)%500 == 0 {
			fmt.Fprintf(os.Stderr, "principals: checked %d of %d members, %d may read\n", i+1, len(members), len(keep))
		}
	}
	return keep, nil
}

// snapshotPath is the permission snapshot the backup writes on every run; it
// may not exist, in which case the resolver simply has no offline fallback.
func snapshotPath(opts *config.Options) string {
	return filepath.Join(opts.ArchiveDir, "server_"+opts.GuildID, "permissions.json")
}

func anyAllowed(allowed map[string]bool, targets []discord.Channel) bool {
	for _, ch := range targets {
		if allowed[ch.ID] {
			return true
		}
	}
	return false
}

// --- members to principals -------------------------------------------------

type principalsOptions struct {
	IncludeBots bool
	NoToken     bool
	KeepNames   bool
}

// principalsResult is what membersToPrincipals produced: the full list, the
// entries it added and the existing ones it renamed.
type principalsResult struct {
	All     []config.Principal
	Added   []config.Principal
	Renamed []config.Principal
}

// membersToPrincipals builds the principal list from the guild members. It is
// additive and idempotent: existing entries keep their token (a running client
// must not be locked out) and are renamed to whatever Discord currently shows,
// members that are already configured are skipped, and everyone else gets a
// fresh random token. It returns the full list plus what changed.
func membersToPrincipals(existing []config.Principal, members []discord.Member, opt principalsOptions) (principalsResult, error) {
	var res principalsResult

	byID := make(map[string]discord.Member, len(members))
	for _, m := range members {
		if _, ok := byID[m.User.ID]; !ok {
			byID[m.User.ID] = m
		}
	}
	// taken holds every identifier already in use (names and aliases), so a
	// generated name cannot collide with one of them.
	taken := map[string]bool{}
	for _, p := range existing {
		taken[strings.ToLower(p.Name)] = true
		for _, a := range p.Aliases {
			taken[strings.ToLower(a)] = true
		}
	}

	configured := map[string]bool{}
	for _, p := range existing {
		configured[p.DiscordID] = true
		if m, ok := byID[p.DiscordID]; ok && !opt.KeepNames {
			if want := uniqueName(m, taken); !strings.EqualFold(want, p.Name) {
				p.Name = want
				res.Renamed = append(res.Renamed, p)
				taken[strings.ToLower(want)] = true
			}
		}
		res.All = append(res.All, p)
	}

	// Deterministic order for name disambiguation, then alphabetical output:
	// the alphabet is what the reader of the config wants to see.
	sorted := append([]discord.Member(nil), members...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return strings.ToLower(principalLabel(sorted[i])) < strings.ToLower(principalLabel(sorted[j]))
	})
	for _, m := range sorted {
		if m.User.Bot && !opt.IncludeBots {
			continue
		}
		if strings.TrimSpace(m.User.ID) == "" || configured[m.User.ID] {
			continue
		}
		p := config.Principal{Name: uniqueName(m, taken), DiscordID: m.User.ID}
		if !opt.NoToken {
			token, err := newPrincipalToken()
			if err != nil {
				return principalsResult{}, err
			}
			p.Token = token
		}
		taken[strings.ToLower(p.Name)] = true
		configured[m.User.ID] = true
		res.Added = append(res.Added, p)
	}
	sort.SliceStable(res.Added, func(i, j int) bool {
		return strings.ToLower(res.Added[i].Name) < strings.ToLower(res.Added[j].Name)
	})
	res.All = append(res.All, res.Added...)
	return res, nil
}

// principalLabel is what the person is called on this server: the server
// nickname ("Alexander Duffeck") first, then the Discord display name, then the
// unique handle, and the ID only as a last resort.
func principalLabel(m discord.Member) string {
	if m.Nick != nil {
		if nick := cleanName(*m.Nick); nick != "" {
			return nick
		}
	}
	for _, s := range []string{m.User.GlobalName, m.User.Username} {
		if s = cleanName(s); s != "" {
			return s
		}
	}
	return m.User.ID
}

// cleanName trims and collapses whitespace: a nickname typed on a phone should
// not carry a newline or a run of spaces into the config.
func cleanName(s string) string { return strings.Join(strings.Fields(s), " ") }

// uniqueName returns the member's name, disambiguated with the handle (and
// ultimately the ID) when somebody else already answers to it. The handle in
// parentheses beats an opaque digit tail because it still says who it is.
func uniqueName(m discord.Member, taken map[string]bool) string {
	base := principalLabel(m)
	if !taken[strings.ToLower(base)] {
		return base
	}
	for _, suffix := range []string{m.User.Username, m.User.ID} {
		suffix = cleanName(suffix)
		if suffix == "" || strings.EqualFold(suffix, base) {
			continue
		}
		if cand := base + " (" + suffix + ")"; !taken[strings.ToLower(cand)] {
			return cand
		}
	}
	for n := 2; ; n++ {
		if cand := fmt.Sprintf("%s (%s %d)", base, m.User.ID, n); !taken[strings.ToLower(cand)] {
			return cand
		}
	}
}

// newPrincipalToken returns 32 random bytes as base64url: 256 bits, safe in a
// header and in JSON.
func newPrincipalToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// checkPrincipals mirrors the "mcp.principals" rules of config.CheckMCP, minus
// the HTTP rules: it runs before anything is printed or written, so a bad list
// is caught here instead of on the next server start.
func checkPrincipals(principals []config.Principal) error {
	names := map[string]string{}
	ids := map[string]string{}
	tokens := map[string]string{}
	for _, p := range principals {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return fmt.Errorf("a principal has no name (discord_id %q)", p.DiscordID)
		}
		if id := strings.TrimSpace(p.DiscordID); id == "" {
			return fmt.Errorf("principal %q has no discord_id", name)
		} else if other, dup := ids[id]; dup {
			return fmt.Errorf("principals %q and %q share the discord_id %s", other, name, id)
		} else {
			ids[id] = name
		}
		key := strings.ToLower(name)
		if other, dup := names[key]; dup {
			return fmt.Errorf("duplicate principal name %q (%s and %s)", name, other, p.DiscordID)
		}
		names[key] = p.DiscordID
		if p.Token != "" {
			if other, dup := tokens[p.Token]; dup {
				return fmt.Errorf("principals %q and %q share the same token", other, name)
			}
			tokens[p.Token] = name
		}
	}
	return nil
}

func countHumans(members []discord.Member) int {
	n := 0
	for _, m := range members {
		if !m.User.Bot {
			n++
		}
	}
	return n
}

func botHint(includeBots bool) string {
	if includeBots {
		return ""
	}
	return " (bots skipped, --include-bots to keep them)"
}

func renamedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d renamed)", n)
}

// printPrincipalsFragment writes a "principals": [ ... ] block indented for the
// "mcp" object, so it can be pasted straight into the config.
func printPrincipalsFragment(w io.Writer, principals []config.Principal) error {
	if principals == nil {
		principals = []config.Principal{} // "null" would be a broken config
	}
	body, err := json.MarshalIndent(principals, "    ", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "    \"principals\": %s\n", body)
	return err
}

// printPrincipalTable lists every principal with its token, for handing tokens
// to the clients they belong to.
func printPrincipalTable(w io.Writer, principals []config.Principal) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tDISCORD_ID\tTOKEN")
	for _, p := range principals {
		token := p.Token
		if token == "" {
			token = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, p.DiscordID, token)
	}
	tw.Flush()
}
