// Package store implements the append-only JSONL storage primitives.
//
// Invariants (the whole incremental logic rests on these):
//   - messages.jsonl holds one compact JSON object per line, ordered
//     ascending by Discord snowflake ID (oldest first).
//   - Lines are only ever appended. Nothing is rewritten, so a crash can at
//     most leave a partial tail line, which TruncatePartial removes.
//   - After append + fsync, data is durable. Before fsync, a crash loses at
//     most the in-flight batch.
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Record is one parsed JSONL line.
type Record struct {
	ID  string
	TS  time.Time
	Raw []byte
}

// ParseFunc extracts id and timestamp from a line. Returning an error marks
// the line as unreadable; callers skip such lines.
type ParseFunc func(line []byte) (id string, ts time.Time, err error)

// EnsureDir creates a directory tree.
func EnsureDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

// TruncatePartial removes an incomplete tail line. Our writers always end
// every line with '\n', so a file that does not end with '\n' must have been
// interrupted mid-write; everything after the last '\n' is discarded.
func TruncatePartial(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() == 0 {
		return nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, fi.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	// Find the last newline and cut everything after it.
	off := fi.Size() - 1
	const chunk = int64(64 << 10)
	buf := make([]byte, chunk)
	for off > 0 {
		n := chunk
		if n > off {
			n = off
		}
		start := off - n
		if _, err := f.ReadAt(buf[:n], start); err != nil {
			return err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return f.Truncate(start + int64(i) + 1)
		}
		off = start
	}
	// No newline at all: the whole file is one partial line.
	return f.Truncate(0)
}

// AppendLines appends lines (without trailing newlines) to path, fsyncs,
// and returns the number of lines written.
func AppendLines(path string, lines [][]byte) (int, error) {
	if len(lines) == 0 {
		return 0, nil
	}
	if err := EnsureDir(filepath.Dir(path)); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	w := 0
	for _, l := range lines {
		l = bytes.TrimRight(l, "\n")
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		if _, err := f.Write(append(l, '\n')); err != nil {
			return w, err
		}
		w++
	}
	if err := f.Sync(); err != nil {
		return w, err
	}
	return w, nil
}

var errNoNewline = errors.New("file does not end with newline; call TruncatePartial first")

// LastLine returns the last non-empty line of path, or nil if the file does
// not exist or is empty. It reads backwards and never loads the whole file.
func LastLine(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size == 0 {
		return nil, nil
	}
	one := make([]byte, 1)
	if _, err := f.ReadAt(one, size-1); err != nil {
		return nil, err
	}
	if one[0] != '\n' {
		return nil, errNoNewline
	}
	// Strip trailing newlines, then find the previous one.
	end := size
	for end > 0 {
		if _, err := f.ReadAt(one, end-1); err != nil {
			return nil, err
		}
		if one[0] != '\n' {
			break
		}
		end--
	}
	if end == 0 {
		return nil, nil
	}
	const chunk = int64(64 << 10)
	off := end
	for off > 0 {
		n := chunk
		if n > off {
			n = off
		}
		start := off - n
		buf := make([]byte, n)
		if _, err := f.ReadAt(buf, start); err != nil {
			return nil, err
		}
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			line := make([]byte, end-(start+int64(i)+1))
			if _, err := f.ReadAt(line, start+int64(i)+1); err != nil {
				return nil, err
			}
			return line, nil
		}
		off = start
	}
	line := make([]byte, end)
	if _, err := f.ReadAt(line, 0); err != nil {
		return nil, err
	}
	return line, nil
}

// LastRecord returns the parsed last line, or nil if the file is empty.
func LastRecord(path string, parse ParseFunc) (*Record, error) {
	line, err := LastLine(path)
	if err != nil {
		return nil, err
	}
	if line == nil {
		return nil, nil
	}
	id, ts, err := parse(line)
	if err != nil {
		return nil, fmt.Errorf("parse last line of %s: %w", path, err)
	}
	cp := append([]byte(nil), line...)
	return &Record{ID: id, TS: ts, Raw: cp}, nil
}

// Recent returns up to maxLines records with ts >= since, in ascending
// order. It scans backwards and stops at the first older line, so cost is
// proportional to the window, not the file size.
func Recent(path string, since time.Time, maxLines int, parse ParseFunc) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size == 0 {
		return nil, nil
	}
	var out []Record // collected newest-first
	var head []byte  // incomplete head fragment carried to the next chunk
	off := size
	done := false
	const chunk = int64(64 << 10)
	for off > 0 && !done {
		n := chunk
		if n > off {
			n = off
		}
		off -= n
		buf := make([]byte, n)
		if _, err := f.ReadAt(buf, off); err != nil {
			return nil, err
		}
		if len(head) > 0 {
			nb := make([]byte, 0, len(buf)+len(head))
			nb = append(nb, buf...)
			nb = append(nb, head...)
			buf = nb
		}
		parts := bytes.Split(buf, []byte{'\n'})
		start := 0
		if off > 0 {
			head = parts[0]
			start = 1
		}
		for i := len(parts) - 1; i >= start; i-- {
			line := parts[i]
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			id, ts, err := parse(line)
			if err != nil {
				continue // skip unreadable lines, never abort the run
			}
			if ts.Before(since) {
				done = true
				break
			}
			cp := append([]byte(nil), line...)
			out = append(out, Record{ID: id, TS: ts, Raw: cp})
			if len(out) >= maxLines {
				done = true
				break
			}
		}
	}
	// Reverse to ascending order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// AllIDs streams the whole file and returns the set of record IDs.
// Only used for rare one-off checks (pin backfills), never on the hot path.
func AllIDs(path string, parse ParseFunc) (map[string]struct{}, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]struct{}{}, nil
		}
		return nil, err
	}
	defer f.Close()
	ids := map[string]struct{}{}
	var frag []byte
	buf := make([]byte, 1<<20)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			data := append(frag, buf[:n]...)
			frag = nil
			lines := bytes.Split(data, []byte{'\n'})
			frag = append([]byte(nil), lines[len(lines)-1]...)
			for _, line := range lines[:len(lines)-1] {
				if len(bytes.TrimSpace(line)) == 0 {
					continue
				}
				id, _, err := parse(line)
				if err != nil {
					continue
				}
				ids[id] = struct{}{}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	if len(bytes.TrimSpace(frag)) > 0 {
		if id, _, err := parse(frag); err == nil {
			ids[id] = struct{}{}
		}
	}
	return ids, nil
}

// CountLines counts non-empty lines. Used once when adopting foreign files;
// steady-state runs track counts incrementally instead.
func CountLines(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	var n int64
	var frag []byte
	buf := make([]byte, 1<<20)
	for {
		rn, rerr := f.Read(buf)
		if rn > 0 {
			data := append(frag, buf[:rn]...)
			frag = nil
			lines := bytes.Split(data, []byte{'\n'})
			frag = append([]byte(nil), lines[len(lines)-1]...)
			for _, line := range lines[:len(lines)-1] {
				if len(bytes.TrimSpace(line)) > 0 {
					n++
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return n, rerr
		}
	}
	if len(bytes.TrimSpace(frag)) > 0 {
		n++
	}
	return n, nil
}

// WriteJSON writes obj as indented JSON atomically (temp file + rename) so
// readers never see a half-written metadata file.
func WriteJSON(path string, obj any) error {
	if err := EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
