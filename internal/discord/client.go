// Package discord is a minimal Discord REST API client.
//
// It deliberately uses only the REST API (no gateway connection), which is
// everything an archive tool needs. A persistent gateway session would add
// complexity and failure modes without any benefit for a cron-driven backup.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const defaultBase = "https://discord.com/api/v10"

// maxBody is an upper bound for API response bodies.
const maxBody = 16 << 20

// Channel types we care about (Discord API channel types).
const (
	ChannelGuildText  = 0
	ChannelVoice      = 2
	ChannelCategory   = 4
	ChannelNews       = 5 // announcement channel
	ChannelStageVoice = 13
	ChannelForum      = 15
)

// Client is a rate-limit aware Discord REST client. It is safe for
// concurrent use, but the backup runner uses it sequentially on purpose:
// sequential paginated reads are the friendliest way to consume the
// Discord API and make rate-limit handling trivially correct.
type Client struct {
	base  string
	token string
	http  *http.Client

	mu          sync.Mutex
	globalUntil time.Time
}

// New creates a client. base may be empty (production API) or point at a
// test server.
func New(token, base string) *Client {
	if base == "" {
		base = defaultBase
	}
	return &Client{
		base:  base,
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

// --- low level -------------------------------------------------------------

type rateLimitedError struct {
	RetryAfter time.Duration
	Global     bool
}

func (e *rateLimitedError) Error() string {
	return fmt.Sprintf("rate limited, retry after %s (global=%v)", e.RetryAfter, e.Global)
}

func parseRetryAfter(resp *http.Response, body []byte) time.Duration {
	// Prefer the JSON body's retry_after (seconds, may be fractional).
	var payload struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.RetryAfter > 0 {
		return time.Duration(payload.RetryAfter * float64(time.Second))
	}
	// Fall back to the Retry-After header.
	if h := resp.Header.Get("Retry-After"); h != "" {
		if f, err := strconv.ParseFloat(h, 64); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	}
	return 2 * time.Second
}

// do performs one request, transparently handling 429s and 5xx with retries.
// Only GET is needed by this tool, so there is no request body to rewind.
func (c *Client) do(ctx context.Context, path string, query url.Values) ([]byte, http.Header, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if err := c.waitGlobal(ctx); err != nil {
			return nil, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Authorization", "Bot "+c.token)
		req.Header.Set("User-Agent", "discord-backup/1.0 (+local-archive)")
		req.Header.Set("Accept", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			if sleep(ctx, time.Duration(attempt+1)*time.Second) != nil {
				return nil, nil, ctx.Err()
			}
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusOK:
			c.observeLimits(resp.Header)
			return body, resp.Header, nil
		case resp.StatusCode == http.StatusTooManyRequests:
			wait := parseRetryAfter(resp, body) + 100*time.Millisecond
			global := resp.Header.Get("X-RateLimit-Global") == "true"
			c.setGlobal(wait, global, resp.Header)
			if sleep(ctx, wait) != nil {
				return nil, nil, ctx.Err()
			}
			lastErr = &rateLimitedError{RetryAfter: wait, Global: global}
			continue
		case resp.StatusCode == http.StatusForbidden:
			return nil, nil, &ForbiddenError{Path: path, Body: snippet(body)}
		case resp.StatusCode == http.StatusNotFound:
			return nil, nil, &NotFoundError{Path: path, Body: snippet(body)}
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("discord API %d on %s: %s", resp.StatusCode, path, snippet(body))
			if sleep(ctx, time.Duration(attempt+1)*time.Second) != nil {
				return nil, nil, ctx.Err()
			}
			continue
		default:
			return nil, nil, fmt.Errorf("discord API %d on %s: %s", resp.StatusCode, path, snippet(body))
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("request failed")
	}
	return nil, nil, fmt.Errorf("GET %s: %w (after retries)", path, lastErr)
}

// ForbiddenError means the bot may not access the resource (HTTP 403).
// Callers treat this as "no access", not as a fatal error.
type ForbiddenError struct {
	Path string
	Body string
}

func (e *ForbiddenError) Error() string { return fmt.Sprintf("forbidden: %s (%s)", e.Path, e.Body) }

// NotFoundError means the resource does not exist (HTTP 404).
// Body carries Discord's {"message","code"} payload for diagnosis.
type NotFoundError struct {
	Path string
	Body string
}

func (e *NotFoundError) Error() string { return fmt.Sprintf("not found: %s (%s)", e.Path, e.Body) }

func snippet(b []byte) string {
	b = bytes.TrimSpace(b)
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) waitGlobal(ctx context.Context) error {
	c.mu.Lock()
	wait := time.Until(c.globalUntil)
	c.mu.Unlock()
	return sleep(ctx, wait)
}

func (c *Client) setGlobal(wait time.Duration, global bool, _ http.Header) {
	if !global {
		return
	}
	c.mu.Lock()
	if until := time.Now().Add(wait); until.After(c.globalUntil) {
		c.globalUntil = until
	}
	c.mu.Unlock()
}

// observeLimits proactively honors per-route buckets: when a response tells
// us the bucket is exhausted, we sleep for the advertised reset time instead
// of hammering the API into a 429.
func (c *Client) observeLimits(h http.Header) {
	if h.Get("X-RateLimit-Global") == "true" {
		if f, err := strconv.ParseFloat(h.Get("Retry-After"), 64); err == nil && f > 0 {
			c.mu.Lock()
			if until := time.Now().Add(time.Duration(f * float64(time.Second))); until.After(c.globalUntil) {
				c.globalUntil = until
			}
			c.mu.Unlock()
		}
		return
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if f, err := strconv.ParseFloat(h.Get("X-RateLimit-Reset-After"), 64); err == nil && f > 0 {
			c.mu.Lock()
			if until := time.Now().Add(time.Duration(f * float64(time.Second))); until.After(c.globalUntil) {
				c.globalUntil = until
			}
			c.mu.Unlock()
		}
	}
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	body, _, err := c.do(ctx, path, query)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// --- API surface -----------------------------------------------------------

// Me validates the token and returns the bot user.
func (c *Client) Me(ctx context.Context) (*User, error) {
	var u User
	if err := c.get(ctx, "/users/@me", nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// Guild fetches guild details including approximate counts.
func (c *Client) Guild(ctx context.Context, guildID string) (*Guild, error) {
	var g Guild
	q := url.Values{"with_counts": {"true"}}
	if err := c.get(ctx, "/guilds/"+guildID, q, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// ListMembers pages through guild members (limit per call, after = user ID).
func (c *Client) ListMembers(ctx context.Context, guildID, after string, limit int) ([]Member, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if after != "" {
		q.Set("after", after)
	}
	var out []Member
	if err := c.get(ctx, "/guilds/"+guildID+"/members", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListChannels returns all channels of a guild.
func (c *Client) ListChannels(ctx context.Context, guildID string) ([]Channel, error) {
	var out []Channel
	if err := c.get(ctx, "/guilds/"+guildID+"/channels", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListMessages returns up to 100 messages. after/before are message IDs,
// at most one should be set. The result order is not relied upon;
// callers sort by ID.
func (c *Client) ListMessages(ctx context.Context, channelID, after, before string, limit int) ([]json.RawMessage, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if after != "" {
		q.Set("after", after)
	}
	if before != "" {
		q.Set("before", before)
	}
	var out []json.RawMessage
	if err := c.get(ctx, "/channels/"+channelID+"/messages", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetMessage fetches a single message (used to backfill pinned messages
// that predate the archive).
func (c *Client) GetMessage(ctx context.Context, channelID, messageID string) (json.RawMessage, error) {
	body, _, err := c.do(ctx, "/channels/"+channelID+"/messages/"+messageID, nil)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode message: %w", err)
	}
	return raw, nil
}

// ListPins returns all pinned messages of a channel.
func (c *Client) ListPins(ctx context.Context, channelID string) ([]json.RawMessage, error) {
	var out []json.RawMessage
	if err := c.get(ctx, "/channels/"+channelID+"/pins", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GuildActiveThreads returns all active threads of the guild that the bot
// can access. This is the v10-correct replacement for the deprecated
// per-channel /channels/{id}/threads/active endpoint (which v10 answers
// with 404). Callers group by ParentID.
func (c *Client) GuildActiveThreads(ctx context.Context, guildID string) ([]Channel, error) {
	var out struct {
		Threads []Channel `json:"threads"`
	}
	if err := c.get(ctx, "/guilds/"+guildID+"/threads/active", nil, &out); err != nil {
		return nil, err
	}
	return out.Threads, nil
}

// ArchivedThreads pages through archived threads. kind is one of
// "public", "private" or "joined" (private threads the bot joined).
// before is an ISO8601 timestamp cursor, empty for the first page.
func (c *Client) ArchivedThreads(ctx context.Context, channelID, kind, before string, limit int) ([]Channel, bool, error) {
	var path string
	switch kind {
	case "public":
		path = "/channels/" + channelID + "/threads/archived/public"
	case "private":
		path = "/channels/" + channelID + "/threads/archived/private"
	case "joined":
		path = "/channels/" + channelID + "/users/@me/threads/archived/private"
	default:
		return nil, false, fmt.Errorf("unknown thread archive kind %q", kind)
	}
	if limit < 1 {
		limit = 100
	}
	if limit > 100 {
		limit = 100
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if before != "" {
		q.Set("before", before)
	}
	var out struct {
		Threads []Channel `json:"threads"`
		HasMore bool      `json:"has_more"`
	}
	if err := c.get(ctx, path, q, &out); err != nil {
		return nil, false, err
	}
	return out.Threads, out.HasMore, nil
}
