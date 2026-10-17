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
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"discord-backup/internal/config"
	"discord-backup/internal/discord"
)

// apiBase is empty in production (the real Discord API); the tests point it at
// a stub server. It is deliberately not configurable: no config key, no flag.
var apiBase string

// runPrincipals lists the guild's members and turns them into
// "mcp.principals" entries: the name and discord_id come from Discord, so
// nobody has to copy user IDs by hand. By default the objects are printed for
// you to paste into the config; --write merges them into the file directly.
func runPrincipals(args []string) int {
	fs := flag.NewFlagSet("principals", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", defaultConfigPath, "configuration file (its token and guild_id are used)")
	includeBots := fs.Bool("include-bots", false, "also emit bot accounts (those are clients, not people)")
	noToken := fs.Bool("no-token", false, `leave out "token": stdio needs none, HTTP requires one per principal`)
	write := fs.Bool("write", false, "merge the result into the config file (atomic, mode 0600)")
	timeout := fs.Duration("timeout", 5*time.Minute, "overall timeout for listing the members")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `discord-backup principals: build "mcp.principals" from the guild member list.

Usage:
  discord-backup principals --config %s
  discord-backup principals --config %s --write

Reads guild_id and token from the same configuration file the backup and the
MCP server use and asks Discord for the member list (this needs the privileged
"Server Members Intent" for the bot). Members that are already configured are
kept exactly as they are, including their tokens, so this never breaks a
running client.

By default the ready-to-paste "principals" block goes to stdout and the human
summary to stderr; --write replaces mcp.principals in the config instead and
prints a name/discord_id/token table so tokens can be handed out.

Options:
`, defaultConfigPath, defaultConfigPath)
		printFlags(fs)
		fmt.Fprintf(os.Stderr, `
Examples:
  # see who Discord knows, then paste the block into the config
  discord-backup principals --config %s

  # or let it write the file (the directory must be writable)
  discord-backup principals --config %s --write
`, defaultConfigPath, defaultConfigPath)
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

	principals, added, err := membersToPrincipals(opts.MCP.Principals, members, principalsOptions{
		IncludeBots: *includeBots,
		NoToken:     *noToken,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "principals: %v\n", err)
		return 1
	}
	if err := checkPrincipals(principals); err != nil {
		fmt.Fprintf(os.Stderr, "principals: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "%s: %d members (%d human), %d already configured, %d new%s\n",
		guild.Name, len(members), countHumans(members), len(principals)-len(added), len(added), botHint(*includeBots))

	if *write {
		if err := config.SavePrincipals(*configPath, principals); err != nil {
			fmt.Fprintf(os.Stderr, "principals: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "wrote %d principals to %s (restart the server: docker compose up -d)\n", len(principals), *configPath)
		printPrincipalTable(os.Stdout, principals)
		return 0
	}

	fmt.Fprintf(os.Stderr, "paste this inside the \"mcp\" object of %s (drop the entries that should not connect)\n", *configPath)
	if err := printPrincipalsFragment(os.Stdout, principals); err != nil {
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

type principalsOptions struct {
	IncludeBots bool
	NoToken     bool
}

// membersToPrincipals builds the principal list from the guild members. It is
// additive and idempotent: existing entries are copied over untouched (their
// tokens must keep working), members that are already configured are skipped,
// and everyone else gets a fresh random token. It returns the full list and
// just the entries it added.
func membersToPrincipals(existing []config.Principal, members []discord.Member, opt principalsOptions) ([]config.Principal, []config.Principal, error) {
	out := make([]config.Principal, 0, len(existing)+len(members))
	configured := map[string]bool{} // discord_id -> already in the config
	taken := map[string]bool{}      // lowercased name -> already used
	for _, p := range existing {
		out = append(out, p)
		configured[strings.TrimSpace(p.DiscordID)] = true
		taken[strings.ToLower(p.Name)] = true
	}

	sorted := append([]discord.Member(nil), members...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return strings.ToLower(sorted[i].User.Username) < strings.ToLower(sorted[j].User.Username)
	})

	var added []config.Principal
	for _, m := range sorted {
		if m.User.Bot && !opt.IncludeBots {
			continue
		}
		if strings.TrimSpace(m.User.ID) == "" || configured[m.User.ID] {
			continue
		}
		p := config.Principal{Name: uniquePrincipalName(m, taken), DiscordID: m.User.ID}
		if !opt.NoToken {
			token, err := newPrincipalToken()
			if err != nil {
				return nil, nil, err
			}
			p.Token = token
		}
		out = append(out, p)
		added = append(added, p)
		configured[m.User.ID] = true
		taken[strings.ToLower(p.Name)] = true
	}
	return out, added, nil
}

// uniquePrincipalName prefers the Discord username, which is unique per
// account. If an existing principal already uses that name, the last four
// digits of the user ID are appended (and the full ID if even that is taken),
// so checkPrincipals never trips over a duplicate.
func uniquePrincipalName(m discord.Member, taken map[string]bool) string {
	name := strings.TrimSpace(m.User.Username)
	if name == "" {
		name = m.User.ID
	}
	if !taken[strings.ToLower(name)] {
		return name
	}
	short := m.User.ID
	if len(short) > 4 {
		short = short[len(short)-4:]
	}
	if cand := name + "-" + short; !taken[strings.ToLower(cand)] {
		return cand
	}
	return name + "-" + m.User.ID
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
