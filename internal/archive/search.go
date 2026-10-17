package archive

import (
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Query describes a message search. All Terms must be present
// (case-insensitive substring), then the filters narrow the result.
type Query struct {
	Terms   []string
	Channel string    // log name (substring) or exact log ID
	Author  string    // author username (substring) or exact user ID
	After   time.Time // only messages at/after this time
	Before  time.Time // only messages strictly before this time
	Limit   int       // max hits returned (default 50)
	Oldest  bool      // order oldest first (default newest first)
}

// Hit is one search result. It carries enough to render without a second
// lookup, plus the message ID for get_message/get_context.
type Hit struct {
	MessageID string    `json:"message_id"`
	LogID     string    `json:"log_id"`
	Channel   string    `json:"channel"`
	Kind      Kind      `json:"kind"`
	Author    string    `json:"author"`
	AuthorID  string    `json:"author_id"`
	TS        time.Time `json:"timestamp"`
	Snippet   string    `json:"snippet"`
	Edited    bool      `json:"edited,omitempty"`
	Deleted   bool      `json:"deleted,omitempty"`
	Attach    bool      `json:"has_attachments,omitempty"`
}

const (
	defaultLimit = 50
	maxLimit     = 500
	maxCollect   = 20000
)

// Search scans the accessible logs and returns matching messages.
// It also returns the total number of matches (which may exceed len(hits)).
func (a *Archive) Search(access Access, q Query) ([]Hit, int) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	channel := strings.ToLower(strings.TrimSpace(q.Channel))
	author := strings.ToLower(strings.TrimSpace(q.Author))
	terms := make([]string, 0, len(q.Terms))
	for _, t := range q.Terms {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			terms = append(terms, t)
		}
	}

	var hits []Hit
	total := 0
	for _, l := range a.logs {
		if l.NoAccess || !access(l.info()) {
			continue
		}
		if channel != "" && l.ID != q.Channel && !strings.Contains(strings.ToLower(l.Name), channel) {
			continue
		}
		for _, d := range l.docs {
			if d.Deleted && !a.opts.ShowDeleted {
				continue
			}
			if !q.After.IsZero() && d.TS.Before(q.After) {
				continue
			}
			if !q.Before.IsZero() && !d.TS.Before(q.Before) {
				continue
			}
			if author != "" && d.AuthorID != q.Author && !strings.Contains(strings.ToLower(d.Author), author) {
				continue
			}
			if !matchAll(d, terms) {
				continue
			}
			total++
			if len(hits) >= maxCollect {
				continue
			}
			hits = append(hits, Hit{
				MessageID: d.ID,
				LogID:     l.ID,
				Channel:   l.Name,
				Kind:      l.Kind,
				Author:    d.Author,
				AuthorID:  d.AuthorID,
				TS:        d.TS,
				Snippet:   snippet(d, terms),
				Edited:    d.Edited,
				Deleted:   d.Deleted,
				Attach:    d.Extra != "",
			})
		}
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].TS.Equal(hits[j].TS) {
			return hits[i].MessageID > hits[j].MessageID
		}
		if q.Oldest {
			return hits[i].TS.Before(hits[j].TS)
		}
		return hits[i].TS.After(hits[j].TS)
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, total
}

func matchAll(d *Doc, terms []string) bool {
	if len(terms) == 0 {
		return true
	}
	hay := d.Lower
	if d.Extra != "" {
		hay = d.Lower + " " + d.Extra
	}
	for _, t := range terms {
		if !strings.Contains(hay, t) {
			return false
		}
	}
	return true
}

// snippet renders a one-line excerpt around the first matching term.
func snippet(d *Doc, terms []string) string {
	const win = 160
	src := d.Content
	if len(d.Lower) != len(d.Content) {
		src = d.Lower // offsets only line up on the lowercased text
	}
	idx := 0
	for _, t := range terms {
		if i := strings.Index(d.Lower, t); i >= 0 && (idx == 0 || i < idx) {
			idx = i
		}
	}
	start := idx - win/2
	if start < 0 {
		start = 0
	}
	if start > len(src) {
		start = len(src)
	}
	end := start + win
	if end > len(src) {
		end = len(src)
	}
	start = runeStart(src, start)
	end = runeStart(src, end)
	s := src[start:end]
	if start > 0 {
		s = "…" + s
	}
	if end < len(src) {
		s = s + "…"
	}
	return collapse(s)
}

func runeStart(s string, i int) int {
	for i > 0 && i < len(s) && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Count returns the number of accessible, non-deleted messages and logs.
func (a *Archive) Count(access Access) (logs, messages int) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, l := range a.logs {
		if l.NoAccess || !access(l.info()) {
			continue
		}
		logs++
		for _, d := range l.docs {
			if d.Deleted && !a.opts.ShowDeleted {
				continue
			}
			messages++
		}
	}
	return logs, messages
}

// Authors returns how many accessible messages each author wrote, most
// frequent first, capped at limit. Nil terms means "all messages".
func (a *Archive) Authors(access Access, terms []string, limit int) []AuthorCount {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if limit <= 0 {
		limit = 20
	}
	lowered := make([]string, 0, len(terms))
	for _, t := range terms {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			lowered = append(lowered, t)
		}
	}
	counts := map[string]*AuthorCount{}
	for _, l := range a.logs {
		if l.NoAccess || !access(l.info()) {
			continue
		}
		for _, d := range l.docs {
			if d.Deleted && !a.opts.ShowDeleted {
				continue
			}
			if !matchAll(d, lowered) {
				continue
			}
			c, ok := counts[d.AuthorID]
			if !ok {
				c = &AuthorCount{AuthorID: d.AuthorID, Author: d.Author}
				counts[d.AuthorID] = c
			}
			c.Messages++
			if d.TS.After(c.Last) {
				c.Last = d.TS
			}
		}
	}
	out := make([]AuthorCount, 0, len(counts))
	for _, c := range counts {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Messages != out[j].Messages {
			return out[i].Messages > out[j].Messages
		}
		return out[i].Author < out[j].Author
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// AuthorCount is one entry of Authors.
type AuthorCount struct {
	AuthorID string    `json:"author_id"`
	Author   string    `json:"author"`
	Messages int       `json:"messages"`
	Last     time.Time `json:"last_message"`
}
