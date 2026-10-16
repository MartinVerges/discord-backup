package attach

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"photo.png":        "photo.png",
		"../../etc/passwd": "_.._etc_passwd", // no separators, no leading dots: harmless
		"..\\windows\\x":   "_windows_x",
		".hidden":          "hidden",
		"a/b\\c:d*e?f\"g":  "a_b_c_d_e_f_g",
		"":                 "file",
		"...":              "file",
		"grüße.pdf":        "gr__e.pdf",
	}
	for in, want := range cases {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(SanitizeFilename(in), "/") || strings.Contains(SanitizeFilename(in), "\\") {
			t.Errorf("sanitize(%q) contains separator", in)
		}
	}
	long := strings.Repeat("a", 200) + ".png"
	if got := SanitizeFilename(long); len(got) > 100 || !strings.HasSuffix(got, ".png") {
		t.Errorf("long name not capped: len=%d", len(got))
	}
}

func TestDefaultAllowHost(t *testing.T) {
	for _, h := range []string{"cdn.discordapp.com", "media.discordapp.net", "images-ext-1.discordapp.net"} {
		if !DefaultAllowHost(h) {
			t.Errorf("should allow %s", h)
		}
	}
	for _, h := range []string{"evil.com", "discordapp.com.evil.com", "http", ""} {
		if DefaultAllowHost(h) {
			t.Errorf("should refuse %s", h)
		}
	}
}

func TestDownloadRoundtrip(t *testing.T) {
	payload := []byte("fake-image-bytes")
	// TLS server: the downloader only fetches https URLs, like the real CDN.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	d := &Downloader{
		HTTP:      srv.Client(),
		MaxBytes:  1 << 20,
		Workers:   2,
		AllowHost: func(string) bool { return true },
	}
	it := Item{URL: srv.URL + "/a.png", MessageID: "m1", AttachmentID: "a1", Filename: "a.png", DestDir: dir}
	ok, skipped, failed := d.Run(t.Context(), []Item{it})
	if ok != 1 || skipped != 0 || failed != 0 {
		t.Fatalf("first run: ok=%d skipped=%d failed=%d", ok, skipped, failed)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "m1_a1_a.png"))
	if err != nil || string(raw) != string(payload) {
		t.Fatalf("stored file: %v %q", err, raw)
	}
	// Second run hits the cache.
	ok, skipped, failed = d.Run(t.Context(), []Item{it})
	if ok != 0 || skipped != 1 || failed != 0 {
		t.Fatalf("second run: ok=%d skipped=%d failed=%d", ok, skipped, failed)
	}
}

func TestDownloadLimits(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 4096))
	}))
	defer srv.Close()

	dir := t.TempDir()
	d := &Downloader{
		HTTP:      srv.Client(),
		MaxBytes:  100, // smaller than payload
		Workers:   1,
		AllowHost: func(string) bool { return true },
	}
	it := Item{URL: srv.URL + "/big.bin", MessageID: "m", AttachmentID: "a", Filename: "big.bin", DestDir: dir}
	_, _, failed := d.Run(t.Context(), []Item{it})
	_ = failed
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("oversized download must leave no file, got %v", entries)
	}

	// Non-allowlisted host is never even requested.
	d2 := &Downloader{AllowHost: func(string) bool { return false }}
	ok, skipped, _ := d2.Run(t.Context(), []Item{it})
	if ok != 0 || skipped != 1 {
		t.Fatalf("refused host: ok=%d skipped=%d", ok, skipped)
	}
}
