package discord

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryAfter429(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if atomic.AddInt64(&hits, 1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"retry_after":0.05,"global":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"1","username":"bot"}`))
	}))
	defer srv.Close()

	c := New("secret", srv.URL)
	u, err := c.Me(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "bot" || atomic.LoadInt64(&hits) != 2 {
		t.Fatalf("user=%+v hits=%d", u, hits)
	}
}

func TestSnowflakeRoundtrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	id := TimeSnowflake(now)
	back, err := SnowflakeTime(id)
	if err != nil {
		t.Fatal(err)
	}
	if back.UnixMilli() != now.UnixMilli() {
		t.Fatalf("roundtrip: %v -> %s -> %v", now, id, back)
	}
	// Chronological order matches ID order.
	a := TimeSnowflake(now.Add(-time.Hour))
	b := TimeSnowflake(now)
	if !LessID(a, b) || LessID(b, a) {
		t.Fatalf("order: %s %s", a, b)
	}
}

func TestCanonicalIgnoresVolatileFields(t *testing.T) {
	msg := `{"id":"1","channel_id":"c","content":"hi","timestamp":"2026-01-01T00:00:00.000Z",` +
		`"author":{"id":"u","username":"alice","bot":false},"member":{"nick":"Ally"},` +
		`"attachments":[],"embeds":[]}`
	renamed := `{"id":"1","channel_id":"c","content":"hi","timestamp":"2026-01-01T00:00:00.000Z",` +
		`"author":{"id":"u","username":"alice2","bot":false},"member":{"nick":"Ally2"},` +
		`"attachments":[],"embeds":[]}`
	edited := `{"id":"1","channel_id":"c","content":"hi!","timestamp":"2026-01-01T00:00:00.000Z",` +
		`"edited_timestamp":"2026-01-01T00:01:00.000Z",` +
		`"author":{"id":"u","username":"alice","bot":false},` +
		`"attachments":[],"embeds":[]}`
	same, err := SameContent(json.RawMessage(msg), json.RawMessage(renamed))
	if err != nil || !same {
		t.Fatalf("nick/username change must not count as edit: %v", err)
	}
	same, err = SameContent(json.RawMessage(msg), json.RawMessage(edited))
	if err != nil || same {
		t.Fatalf("content change must count as edit: %v", err)
	}
}

func TestEnvelopeBotFilter(t *testing.T) {
	raw := json.RawMessage(`{"id":"9","channel_id":"c","content":"x",` +
		`"timestamp":"2026-01-01T00:00:00.000Z",` +
		`"author":{"id":"u","username":"hook","bot":false},"webhook_id":"123",` +
		`"attachments":[]}`)
	env, err := ParseEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !env.IsBotLike() {
		t.Fatal("webhook message must be bot-like")
	}
}
