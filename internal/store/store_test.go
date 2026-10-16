package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func parseTS(line []byte) (string, time.Time, error) {
	// test lines: {"id":"<id>","ts":"<rfc3339>"}
	var v struct {
		ID string `json:"id"`
		TS string `json:"ts"`
	}
	if err := json.Unmarshal(line, &v); err != nil {
		return "", time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, v.TS)
	if err != nil {
		return "", time.Time{}, err
	}
	return v.ID, t, nil
}

func TestAppendLastRecent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "m.jsonl")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(id string, h int) []byte {
		return []byte(`{"id":"` + id + `","ts":"` + base.Add(time.Duration(h)*time.Hour).Format(time.RFC3339) + `"}`)
	}
	lines := [][]byte{mk("1", 1), mk("2", 2), mk("3", 3), mk("4", 4)}
	if n, err := AppendLines(p, lines); err != nil || n != 4 {
		t.Fatalf("append: n=%d err=%v", n, err)
	}
	last, err := LastLine(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(last) != string(lines[3]) {
		t.Fatalf("last line mismatch: %s", last)
	}
	rec, err := LastRecord(p, parseTS)
	if err != nil || rec.ID != "4" {
		t.Fatalf("last record: %+v %v", rec, err)
	}
	// Window covering last 2.5h -> ids 2,3,4 ascending.
	got, err := Recent(p, base.Add(90*time.Minute), 100, parseTS)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "2" || got[1].ID != "3" || got[2].ID != "4" {
		t.Fatalf("recent mismatch: %+v", got)
	}
	// maxLines truncation keeps the newest.
	got, err = Recent(p, base, 2, parseTS)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "3" || got[1].ID != "4" {
		t.Fatalf("recent cap mismatch: %+v", got)
	}
}

func TestTruncatePartial(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.jsonl")
	full := []byte("{\"a\":1}\n{\"b\":2}\n")
	if err := os.WriteFile(p, append(full, []byte(`{"c":3`)...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := TruncatePartial(p); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(full) {
		t.Fatalf("after truncate: %q", got)
	}
	// Complete file is untouched.
	if err := TruncatePartial(p); err != nil {
		t.Fatal(err)
	}
	// Missing file is fine.
	if err := TruncatePartial(filepath.Join(dir, "nope.jsonl")); err != nil {
		t.Fatal(err)
	}
	// LastLine refuses a torn file so callers must truncate first.
	if err := os.WriteFile(p, []byte("{\"x\": partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LastLine(p); err == nil {
		t.Fatal("expected error for torn file")
	}
}

func TestAllIDsAndCount(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.jsonl")
	if _, err := AppendLines(p, [][]byte{
		[]byte(`{"id":"a","ts":"2026-01-01T00:00:00Z"}`),
		[]byte(`{"id":"b","ts":"2026-01-01T01:00:00Z"}`),
		[]byte(``), // empty lines are ignored
	}); err != nil {
		t.Fatal(err)
	}
	ids, err := AllIDs(p, parseTS)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids: %v", ids)
	}
	n, err := CountLines(p)
	if err != nil || n != 2 {
		t.Fatalf("count: %d %v", n, err)
	}
}

func TestWriteJSONAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "info.json")
	if err := WriteJSON(p, map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{\n  \"a\": \"b\"\n}\n" {
		t.Fatalf("json: %q", raw)
	}
}
