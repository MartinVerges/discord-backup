package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// renameFile is os.Rename; a test replaces it to exercise the bind-mount
// fallback below without needing an actual mount.
var renameFile = os.Rename

// SavePrincipals replaces mcp.principals in the configuration file, leaving
// every other value untouched. It is the only place this tool writes the
// configuration, and it does so carefully:
//
//   - the file is staged next to itself and renamed into place, so a crash
//     cannot leave a truncated config behind;
//   - the result is 0600 from the first byte, because the principals' tokens
//     live in it and Load refuses a token-bearing file with a looser mode;
//   - the rest of the document is preserved as written (keys are re-ordered,
//     which the format cannot avoid, but no value is touched).
//
// A container usually bind-mounts the config as its own mount point. Such a
// file cannot be renamed over ("device or resource busy"), and even a rename
// that succeeded would not be visible through the mount, because it replaces
// the inode while the mount keeps the old one. Whenever the staged file cannot
// be created or renamed into place (a mounted file, a read-only directory such
// as the image's /etc), the file is rewritten in place instead and inPlace is
// true: its mode and owner survive, but the write is not atomic, so a caller
// may want to say so.
//
// The rename error is not inspected: EBUSY (a mount point), EXDEV and EACCES
// (a read-only directory) all mean the same thing here.
func SavePrincipals(path string, principals []Principal) (inPlace bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, readErr(path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep integers exactly as written
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return false, fmt.Errorf("%s: parse: %w", path, err)
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
		return false, err
	}
	out = append(out, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err == nil {
		defer os.Remove(tmp.Name()) // no-op once renamed
		if err = writeWhole(tmp, out); err == nil {
			// CreateTemp already created the file as 0600, so the rename
			// publishes it with the right mode; the owner becomes whoever ran
			// the command.
			if err = renameFile(tmp.Name(), path); err == nil {
				return false, nil
			}
		}
	}
	// Either the directory is not writable (a bind-mounted config in a
	// read-only /etc) or the file is a mount point. Rewriting it in place is
	// the only thing left - and for a mounted file it is the only thing that
	// works at all.
	if werr := overwrite(path, out); werr != nil {
		const hint = `a bind-mounted config must be writable: drop ":ro", chown it to the user running the command, or mount its directory instead of the file`
		if err != nil {
			return false, fmt.Errorf("write %s: %v; writing in place: %w (%s)", path, err, werr, hint)
		}
		return false, fmt.Errorf("write %s: %w (%s)", path, werr, hint)
	}
	return true, nil
}

// writeWhole writes, flushes and closes a staged file. A short write would
// otherwise go unnoticed until the rename publishes half a config.
func writeWhole(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// overwrite truncates an existing file and rewrites it under its own name,
// which keeps its inode, mode and owner - and is the only way to change the
// content of a bind-mounted file.
func overwrite(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	return writeWhole(f, data)
}
