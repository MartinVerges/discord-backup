package discord

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// --- types -----------------------------------------------------------------

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

type Guild struct {
	ID                   string  `json:"id"`
	Name                 string  `json:"name"`
	Description          *string `json:"description"`
	Icon                 *string `json:"icon"`
	ApproximateMembers   int     `json:"approximate_member_count"`
	ApproximatePresences int     `json:"approximate_presence_count"`
}

type ThreadMetadata struct {
	Archived         bool   `json:"archived"`
	ArchiveTimestamp string `json:"archive_timestamp"`
	Locked           bool   `json:"locked"`
}

type Channel struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Type         int             `json:"type"`
	Topic        *string         `json:"topic"`
	Position     int             `json:"position"`
	ParentID     *string         `json:"parent_id"`
	OwnerID      *string         `json:"owner_id"`
	MessageCount *int            `json:"message_count"`
	ThreadMeta   *ThreadMetadata `json:"thread_metadata"`
}

type Member struct {
	User User    `json:"user"`
	Nick *string `json:"nick"`
}

// Envelope is the minimal typed view of a message used for filtering,
// cursor tracking and change detection. The full raw payload is what gets
// archived, so nothing is lost even if this struct ignores new fields.
type Envelope struct {
	ID              string           `json:"id"`
	ChannelID       string           `json:"channel_id"`
	Content         string           `json:"content"`
	Timestamp       time.Time        `json:"timestamp"`
	EditedTimestamp *time.Time       `json:"edited_timestamp"`
	Pinned          bool             `json:"pinned"`
	Author          User             `json:"author"`
	WebhookID       *string          `json:"webhook_id"`
	Attachments     []AttachmentMeta `json:"attachments"`
}

// AttachmentMeta describes one message attachment.
type AttachmentMeta struct {
	ID          string  `json:"id"`
	Filename    string  `json:"filename"`
	Size        int64   `json:"size"`
	URL         string  `json:"url"`
	ContentType *string `json:"content_type"`
}

// ParseEnvelope extracts the comparable fields from a raw message.
func ParseEnvelope(raw json.RawMessage) (*Envelope, error) {
	var e Envelope
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("parse message envelope: %w", err)
	}
	if e.ID == "" {
		return nil, fmt.Errorf("message without id")
	}
	if e.Timestamp.IsZero() {
		return nil, fmt.Errorf("message %s without timestamp", e.ID)
	}
	return &e, nil
}

// IsBotLike reports whether the message looks bot-generated.
// Webhook messages are treated like bot messages: bridges and feeds would
// otherwise pollute a human archive, and the flag is explicit anyway.
func (e *Envelope) IsBotLike() bool {
	return e.Author.Bot || e.WebhookID != nil
}

// --- canonical comparison --------------------------------------------------

// canonical is the subset of a message that counts as "content" for edit
// detection. Volatile metadata (author avatar, member nick, mention lists
// with embedded member objects) is intentionally excluded so nickname
// changes do not register as message edits.
type canonical struct {
	Content         string            `json:"content"`
	EditedTimestamp *time.Time        `json:"edited_timestamp"`
	Attachments     []AttachmentMeta  `json:"attachments"`
	Embeds          []json.RawMessage `json:"embeds"`
	StickerItems    []json.RawMessage `json:"sticker_items"`
	Poll            json.RawMessage   `json:"poll,omitempty"`
	Flags           int               `json:"flags"`
	Pinned          bool              `json:"pinned"`
}

// Canonical returns a stable fingerprint of the message content.
// Two fetches of an unchanged message must yield identical bytes.
func Canonical(raw json.RawMessage) ([]byte, error) {
	var c canonical
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("canonicalize message: %w", err)
	}
	if c.Attachments == nil {
		c.Attachments = []AttachmentMeta{}
	}
	if c.Embeds == nil {
		c.Embeds = []json.RawMessage{}
	}
	if c.StickerItems == nil {
		c.StickerItems = []json.RawMessage{}
	}
	out, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical message: %w", err)
	}
	return out, nil
}

// SameContent reports whether two raw messages carry identical content.
func SameContent(a, b json.RawMessage) (bool, error) {
	ca, err := Canonical(a)
	if err != nil {
		return false, err
	}
	cb, err := Canonical(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ca, cb), nil
}

// CompactLine compacts raw JSON so it fits on a single JSONL line.
func CompactLine(raw json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, fmt.Errorf("compact message: %w", err)
	}
	return buf.Bytes(), nil
}

// --- snowflakes ------------------------------------------------------------

// discordEpochMs is 2015-01-01T00:00:00Z in milliseconds.
const discordEpochMs = 1420070400000

// SnowflakeTime extracts the creation time embedded in a Discord snowflake.
func SnowflakeTime(id string) (time.Time, error) {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid snowflake %q: %w", id, err)
	}
	ms := int64(n>>22) + discordEpochMs
	return time.UnixMilli(ms).UTC(), nil
}

// TimeSnowflake returns the smallest snowflake whose timestamp is >= t.
// It is used to turn the edit-window duration into an `after=` cursor.
func TimeSnowflake(t time.Time) string {
	ms := t.UTC().UnixMilli() - discordEpochMs
	if ms < 0 {
		ms = 0
	}
	return strconv.FormatUint(uint64(ms)<<22, 10)
}

// LessID compares two snowflake IDs numerically (chronological order).
func LessID(a, b string) bool {
	// Snowflakes from the same era share length; numeric compare via length
	// first avoids big-int parsing for the hot path.
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
