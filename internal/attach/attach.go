// Package attach downloads message attachments to local disk.
//
// Security properties:
//   - Only URLs on Discord's CDN hosts are fetched (allowlist).
//   - Remote filenames are sanitized; the stored name is prefixed with the
//     message and attachment IDs, so a malicious name like "../../x" can
//     never escape the destination directory.
//   - Downloads go to a temp file and are renamed only on success, with a
//     hard size cap enforced while streaming.
package attach

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Item is one file to download.
type Item struct {
	URL          string
	MessageID    string
	AttachmentID string
	Filename     string
	DestDir      string
}

// Downloader fetches attachments with bounded parallelism.
type Downloader struct {
	HTTP     *http.Client
	MaxBytes int64
	Workers  int
	Log      *slog.Logger

	// AllowHost overrides the default CDN allowlist (used in tests).
	AllowHost func(host string) bool
}

func (d *Downloader) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (d *Downloader) workers() int {
	if d.Workers > 0 {
		return d.Workers
	}
	return 4
}

// DefaultAllowHost accepts Discord CDN hosts only.
func DefaultAllowHost(host string) bool {
	h := strings.ToLower(host)
	if h == "cdn.discordapp.com" || h == "media.discordapp.net" {
		return true
	}
	if strings.HasSuffix(h, ".discordapp.com") || strings.HasSuffix(h, ".discordapp.net") {
		return true
	}
	return false
}

func (d *Downloader) allow(host string) bool {
	if d.AllowHost != nil {
		return d.AllowHost(host)
	}
	return DefaultAllowHost(host)
}

// SanitizeFilename maps an untrusted remote name to a safe path segment:
// only [A-Za-z0-9._-] survive, everything else becomes '_', leading dots
// are stripped (no hidden files, no ".."), length is capped.
func SanitizeFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := strings.TrimLeft(b.String(), ".")
	s = strings.TrimSpace(s)
	if len(s) > 100 {
		// Keep the extension readable: cut the stem, keep the suffix.
		ext := filepath.Ext(s)
		stem := s[:len(s)-len(ext)]
		keep := 100 - len(ext)
		if keep < 1 {
			s = s[:100]
		} else {
			if len(stem) > keep {
				stem = stem[:keep]
			}
			s = stem + ext
		}
	}
	if s == "" {
		s = "file"
	}
	return s
}

// DestName returns the on-disk filename for an attachment.
func DestName(messageID, attachmentID, filename string) string {
	return messageID + "_" + attachmentID + "_" + SanitizeFilename(filename)
}

// Run downloads all items, skipping files that already exist.
// A single failure never aborts the batch; errors are logged and counted.
func (d *Downloader) Run(ctx context.Context, items []Item) (ok, skipped, failed int) {
	if len(items) == 0 {
		return 0, 0, 0
	}
	ch := make(chan Item)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < d.workers(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range ch {
				st, err := d.one(ctx, it)
				mu.Lock()
				switch st {
				case statusOK:
					ok++
				case statusSkipped:
					skipped++
				default:
					failed++
					_ = err
				}
				mu.Unlock()
			}
		}()
	}
	for _, it := range items {
		select {
		case <-ctx.Done():
			break
		case ch <- it:
		}
	}
	close(ch)
	wg.Wait()
	return ok, skipped, failed
}

type dlStatus int

const (
	statusOK dlStatus = iota
	statusSkipped
	statusFailed
)

func (d *Downloader) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func (d *Downloader) one(ctx context.Context, it Item) (dlStatus, error) {
	u, err := url.Parse(it.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		d.log().Warn("attachment: refusing non-https/invalid URL", "msg", it.MessageID)
		return statusSkipped, nil
	}
	if !d.allow(u.Host) {
		d.log().Warn("attachment: host not on CDN allowlist", "host", u.Host, "msg", it.MessageID)
		return statusSkipped, nil
	}
	if err := os.MkdirAll(it.DestDir, 0o755); err != nil {
		d.log().Error("attachment: mkdir failed", "err", err)
		return statusFailed, err
	}
	dest := filepath.Join(it.DestDir, DestName(it.MessageID, it.AttachmentID, it.Filename))
	// Never write outside DestDir, even if something above misbehaves.
	if filepath.Dir(dest) != it.DestDir {
		d.log().Warn("attachment: escaping filename rejected", "msg", it.MessageID)
		return statusSkipped, nil
	}
	if fi, err := os.Stat(dest); err == nil && fi.Size() > 0 {
		return statusSkipped, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, it.URL, nil)
	if err != nil {
		return statusFailed, err
	}
	req.Header.Set("User-Agent", "discord-backup/1.0 (+local-archive)")
	resp, err := d.client().Do(req)
	if err != nil {
		d.log().Warn("attachment: download failed", "url", it.URL, "err", err)
		return statusFailed, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		d.log().Warn("attachment: CDN rate limited, skipping (next run retries)", "msg", it.MessageID)
		return statusFailed, fmt.Errorf("cdn 429")
	}
	if resp.StatusCode != http.StatusOK {
		d.log().Warn("attachment: bad status", "status", resp.StatusCode, "msg", it.MessageID)
		return statusFailed, fmt.Errorf("cdn status %d", resp.StatusCode)
	}
	if resp.ContentLength > d.maxBytes() {
		d.log().Warn("attachment: too large, skipping", "size", resp.ContentLength, "msg", it.MessageID)
		return statusSkipped, nil
	}
	tmp, err := os.CreateTemp(it.DestDir, ".dl-*")
	if err != nil {
		return statusFailed, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	// Enforce the cap while streaming (Content-Length may lie or be absent).
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, d.maxBytes()+1))
	if err != nil {
		tmp.Close()
		return statusFailed, err
	}
	if n > d.maxBytes() {
		tmp.Close()
		d.log().Warn("attachment: exceeded size cap, discarded", "msg", it.MessageID)
		return statusSkipped, nil
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return statusFailed, err
	}
	if err := tmp.Close(); err != nil {
		return statusFailed, err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return statusFailed, err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return statusFailed, err
	}
	return statusOK, nil
}

func (d *Downloader) maxBytes() int64 {
	if d.MaxBytes > 0 {
		return d.MaxBytes
	}
	return 50 << 20
}
