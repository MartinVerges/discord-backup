package backup

import (
	"regexp"
	"strings"
	"testing"

	"discord-backup/internal/discord"
)

func strptr(s string) *string { return &s }

var fieldSep = regexp.MustCompile(` {2,}`)

func TestPrintChannelsAligned(t *testing.T) {
	cats := map[string]string{"P1": "Internal Chat", "P2": "Development"}
	channels := []discord.Channel{
		{ID: "3", Name: "vacation-and-awesome-landscapes", Type: 0, Position: 18, ParentID: strptr("P1"), Topic: strptr("post your pics")},
		{ID: "1", Name: "random", Type: 0, Position: 2, ParentID: strptr("P1")},
		{ID: "2", Name: "dev", Type: 0, Position: 26, ParentID: strptr("P2")},
		{ID: "4", Name: "hangout", Type: 2, Position: 2, ParentID: strptr("P1")},
	}
	r := &Runner{}
	var sb strings.Builder
	r.printChannels(&sb, channels, cats)
	out := sb.String()
	t.Logf("\n%s", out)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 7 { // header + 4 channels + blank + summary
		t.Fatalf("lines: %q", out)
	}
	// Every data row must split into the same left-aligned fields, and
	// the ID field must start at the same byte offset in every row.
	idOff := -1
	for _, l := range lines[1:5] {
		fields := fieldSep.Split(strings.TrimRight(l, " "), -1)
		if len(fields) < 4 {
			t.Fatalf("row has %d fields, want >=4: %q", len(fields), l)
		}
		off := strings.Index(l, fields[3])
		if idOff < 0 {
			idOff = off
		} else if off != idOff {
			t.Fatalf("ID column misaligned:\n%s", out)
		}
	}
	// Grouped by category (alphabetical), then position: Development first.
	if !strings.Contains(lines[1], "Development") || !strings.Contains(lines[2], "Internal Chat") {
		t.Fatalf("not grouped by category:\n%s", out)
	}
	if !strings.Contains(out, "4 channels") {
		t.Fatalf("missing summary:\n%s", out)
	}
	// Readable type names and a real TOPIC column.
	if !strings.Contains(out, "voice") || !strings.Contains(out, "post your pics") {
		t.Fatalf("type/topic columns wrong:\n%s", out)
	}
}
