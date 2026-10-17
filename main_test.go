package main

import "testing"

// TestProbeAddr pins the healthcheck's address handling: a wildcard bind
// address must be probed on loopback, a real host must be kept.
func TestProbeAddr(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:8765": "127.0.0.1:8765",
		"0.0.0.0:8765":   "127.0.0.1:8765",
		":8765":          "127.0.0.1:8765",
		"[::]:8765":      "127.0.0.1:8765",
		"[::1]:8765":     "[::1]:8765",
		"example.com:9":  "example.com:9",
		"no-port":        "no-port",
	} {
		if got := probeAddr(in); got != want {
			t.Errorf("probeAddr(%q) = %q, want %q", in, got, want)
		}
	}
}
