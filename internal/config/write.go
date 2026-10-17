package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SavePrincipals replaces mcp.principals in the configuration file, leaving
// every other value untouched. It is the only place this tool writes the
// configuration, and it does so carefully:
//
//   - the file is renamed into place, so a crash cannot leave a truncated
//     config behind;
//   - the result is 0600 from the first byte, because the principals' tokens
//     live in it and Load refuses a token-bearing file with a looser mode;
//   - the rest of the document is preserved as written (keys are re-ordered,
//     which the format cannot avoid, but no value is touched).
//
// The directory must be writable: the write happens next to the file.
func SavePrincipals(path string, principals []Principal) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return readErr(path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep integers exactly as written
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("%s: parse: %w", path, err)
	}
	mcp, ok := doc["mcp"].(map[string]any)
	if !ok {
		mcp = map[string]any{}
	}
	if principals == nil {
		principals = []Principal{} // "null" is not a principals list
	}
	mcp["principals"] = principals
	doc["mcp"] = mcp

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("write %s: %w (the directory must be writable for --write)", path, err)
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	// CreateTemp already created the file as 0600, so the rename publishes it
	// with the right mode; the owner becomes whoever ran the command.
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
