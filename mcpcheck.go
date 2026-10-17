package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"discord-backup/internal/config"
)

// errUnauthorized is reported when the endpoint rejects the bearer token.
var errUnauthorized = errors.New("401 Unauthorized: the principal's token was rejected")

// mcpCheck is what a smoke test learned from a running MCP endpoint.
type mcpCheck struct {
	Server    string
	Version   string
	Protocol  string
	Tools     []string
	Principal string
	DiscordID string
	Channels  int
	Threads   int
	Messages  int
	Source    string
}

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// mcpSmoke does an authenticated JSON-RPC round trip: initialize, tools/list and
// a whoami call. Together they prove that the endpoint is reachable, that the
// principal's token is accepted, that the archive is loaded and that permission
// resolution works - the useful half of "is my MCP actually serving me?".
func mcpSmoke(client *http.Client, base, token string) (mcpCheck, error) {
	var out mcpCheck

	call := func(id int, method string, params any, into any) error {
		payload, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": method, "params": params,
		})
		if err != nil {
			return err
		}
		req, err := http.NewRequest(http.MethodPost, base+"/mcp", bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return errUnauthorized
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: %s", method, resp.Status)
		}
		var reply rpcReply
		if err := json.Unmarshal(body, &reply); err != nil {
			return fmt.Errorf("%s: unexpected response: %w", method, err)
		}
		if reply.Error != nil {
			return fmt.Errorf("%s: rpc error %d: %s", method, reply.Error.Code, reply.Error.Message)
		}
		if into != nil {
			if err := json.Unmarshal(reply.Result, into); err != nil {
				return fmt.Errorf("%s: decode result: %w", method, err)
			}
		}
		return nil
	}

	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := call(1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"clientInfo":      map[string]any{"name": "discord-backup healthcheck", "version": version},
		"capabilities":    map[string]any{},
	}, &init); err != nil {
		return out, err
	}
	out.Server, out.Version, out.Protocol = init.ServerInfo.Name, init.ServerInfo.Version, init.ProtocolVersion

	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := call(2, "tools/list", nil, &list); err != nil {
		return out, err
	}
	for _, t := range list.Tools {
		out.Tools = append(out.Tools, t.Name)
	}

	// whoami touches the archive and the permission resolver, so it fails
	// loudly when the archive is unreadable or no access can be computed.
	var call3 struct {
		IsError    bool            `json:"isError"`
		Structured json.RawMessage `json:"structuredContent"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := call(3, "tools/call", map[string]any{
		"name": "whoami", "arguments": map[string]any{},
	}, &call3); err != nil {
		return out, err
	}
	if call3.IsError {
		msg := "tool error"
		if len(call3.Content) > 0 {
			msg = call3.Content[0].Text
		}
		return out, fmt.Errorf("whoami: %s", msg)
	}
	raw := call3.Structured
	if len(raw) == 0 && len(call3.Content) > 0 {
		raw = json.RawMessage(call3.Content[0].Text)
	}
	var who struct {
		Principal string `json:"principal"`
		DiscordID string `json:"discord_id"`
		Channels  int    `json:"visible_channels"`
		Threads   int    `json:"visible_threads"`
		Messages  int    `json:"visible_messages"`
		Source    string `json:"permission_source"`
	}
	if err := json.Unmarshal(raw, &who); err != nil {
		return out, fmt.Errorf("whoami: decode result: %w", err)
	}
	out.Principal, out.DiscordID = who.Principal, who.DiscordID
	out.Channels, out.Threads, out.Messages, out.Source = who.Channels, who.Threads, who.Messages, who.Source
	return out, nil
}

// printMCPCheck writes the human-readable summary of a successful smoke test.
func printMCPCheck(w io.Writer, base string, c mcpCheck) {
	fmt.Fprintf(w, "%s/mcp ok: %s %s (protocol %s)\n", base, c.Server, c.Version, c.Protocol)
	fmt.Fprintf(w, "  tools (%d): %s\n", len(c.Tools), strings.Join(c.Tools, ", "))
	fmt.Fprintf(w, "  authenticated as %s (%s)\n", c.Principal, c.DiscordID)
	fmt.Fprintf(w, "  visible: %d channels, %d threads, %d messages (permissions: %s)\n",
		c.Channels, c.Threads, c.Messages, c.Source)
}

// pickPrincipal resolves the principal for the smoke test: a name/email/alias/
// discord_id, or "auto" for the first one that carries an HTTP token.
func pickPrincipal(principals []config.Principal, identifier string) (*config.Principal, error) {
	if strings.EqualFold(identifier, "auto") {
		for i := range principals {
			if strings.TrimSpace(principals[i].Token) != "" {
				return &principals[i], nil
			}
		}
		return nil, errors.New(`no principal in the config has a "token"`)
	}
	return config.ResolvePrincipal(principals, identifier)
}
