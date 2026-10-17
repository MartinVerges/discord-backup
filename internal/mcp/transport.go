package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// ServeStdio runs the newline-delimited JSON-RPC loop used by local MCP
// clients (Claude Desktop, Claude Code, ...) on stdin/stdout.
func (s *Server) ServeStdio(ctx context.Context, principal *Principal) error {
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	for {
		var req Request
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		resp := s.Handle(ctx, principal, &req)
		if resp == nil {
			continue // notification
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
}

// Handler returns the HTTP handler (JSON or SSE per request Accept header).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleHTTP)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok"}`)
	})
	return mux
}

// ServeHTTP serves the HTTP transport until ctx is cancelled.
func (s *Server) ServeHTTP(ctx context.Context, addr string, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Info("mcp http listening", "addr", ln.Addr().String())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
	case http.MethodGet, http.MethodDelete:
		// Streamable HTTP clients may open a GET stream; we are stateless and
		// answer everything on POST, so tell them there is nothing to stream.
		w.Header().Set("Allow", "POST")
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	default:
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	principal, ok := s.principalFromRequest(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, rpcErrReply(nil, -32700, "cannot read body"))
		return
	}
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, rpcErrReply(nil, -32700, "empty body"))
		return
	}

	// Batch request.
	if body[0] == '[' {
		var reqs []Request
		if err := json.Unmarshal(body, &reqs); err != nil {
			writeJSON(w, http.StatusBadRequest, rpcErrReply(nil, -32700, "parse error"))
			return
		}
		var out []*Response
		for i := range reqs {
			if resp := s.Handle(r.Context(), principal, &reqs[i]); resp != nil {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted) // only notifications
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcErrReply(nil, -32700, "parse error"))
		return
	}
	resp := s.Handle(r.Context(), principal, &req)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted) // notification
		return
	}
	if wantsSSE(r) {
		writeSSE(w, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// principalFromRequest authenticates the caller via a bearer token.
func (s *Server) principalFromRequest(r *http.Request) (*Principal, bool) {
	auth := r.Header.Get("Authorization")
	token := ""
	if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		token = strings.TrimSpace(auth[7:])
	}
	if token == "" {
		token = r.Header.Get("X-MCP-Token")
	}
	return s.settings.ByToken(token)
}

func wantsSSE(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeSSE(w http.ResponseWriter, resp *Response) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	raw, err := json.Marshal(resp)
	if err != nil {
		return
	}
	flusher, _ := w.(http.Flusher)
	io.WriteString(w, "event: message\ndata: ")
	w.Write(raw)
	io.WriteString(w, "\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
