package cctl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Markers that delimit a cctl-managed block inside a tool config. They include
// the literal phrase the linter can grep for so the user can audit their
// configs and re-run init to refresh.
const (
	managedBeginMarker = "# >>> cctl-managed block (do not edit between markers) >>>"
	managedEndMarker   = "# <<< cctl-managed block <<<"
)

// upsertManagedBlockLocal reads path, replaces the cctl-managed block (or
// appends one if absent), and writes the file atomically. Returns whether
// anything changed.
//
// `body` must NOT include the marker lines — they're added here. Missing
// parent dirs are created. Missing files are created with 0o644.
func upsertManagedBlockLocal(path, body string) (changed bool, err error) {
	path = expandPath(path)
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	updated, changed := mergeManagedBlock(string(existing), body)
	if !changed {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".cctl-tmp"
	if err := os.WriteFile(tmp, []byte(updated), 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return true, nil
}

// mergeManagedBlock takes existing file contents and the new body to manage,
// returning the merged contents and whether they differ from the input. The
// output is normalised so a second call with the same body is a no-op:
//
//   - head: existing content above the block, trimmed of trailing newlines
//     and then followed by exactly one blank line when non-empty.
//   - block: the managed block, always terminated with a single newline.
//   - tail: existing content below the block, with leading newlines trimmed
//     and re-prefixed with a single newline if non-empty.
//
// When only one marker is present (corrupt/manually-edited file), we refuse
// to touch the file and return changed=false.
func mergeManagedBlock(existing, body string) (string, bool) {
	wanted := managedBeginMarker + "\n" + strings.TrimRight(body, "\n") + "\n" + managedEndMarker + "\n"

	startIdx := strings.Index(existing, managedBeginMarker)
	endIdx := strings.Index(existing, managedEndMarker)

	var head, tail string
	switch {
	case startIdx < 0 && endIdx < 0:
		head = existing
	case startIdx >= 0 && endIdx > startIdx:
		head = existing[:startIdx]
		endLine := endIdx + len(managedEndMarker)
		if endLine < len(existing) && existing[endLine] == '\n' {
			endLine++
		}
		tail = existing[endLine:]
	default:
		return existing, false
	}

	head = strings.TrimRight(head, "\n")
	if head != "" {
		head += "\n\n"
	}
	tail = strings.TrimLeft(tail, "\n")
	if tail != "" {
		tail = "\n" + tail
	}
	rebuilt := head + wanted + tail
	return rebuilt, rebuilt != existing
}

// upsertManagedBlockRemote applies the same merge on a server via the shell.
// It uses base64 to round-trip the new content safely (no quoting hell) and
// writes atomically via a temp file + rename.
func upsertManagedBlockRemote(s Server, remotePath, body string) (changed bool, err error) {
	// Read current contents (empty if missing).
	readCmd := fmt.Sprintf(`cat %s 2>/dev/null || true`, shellPath(remotePath))
	existing, err := runRemote(s, readCmd)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", remotePath, err)
	}
	updated, changed := mergeManagedBlock(existing, body)
	if !changed {
		return false, nil
	}
	// base64-encode and pipe in via stdin to avoid shell-quoting the body.
	encoded := encodeBase64(updated)
	writeCmd := fmt.Sprintf(
		`mkdir -p "$(dirname %s)" && tmp=%s.cctl-tmp && printf %%s %s | base64 --decode > "$tmp" && mv "$tmp" %s`,
		shellPath(remotePath), shellPath(remotePath), shellQuote(encoded), shellPath(remotePath),
	)
	if _, err := runRemote(s, writeCmd); err != nil {
		return true, fmt.Errorf("write %s: %w", remotePath, err)
	}
	return true, nil
}

// encodeBase64 produces a stable single-line base64 string suitable for
// embedding into a shell command argument.
func encodeBase64(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b := []byte(s)
	out := make([]byte, 0, (len(b)+2)/3*4)
	for i := 0; i < len(b); i += 3 {
		n := 0
		switch {
		case i+2 < len(b):
			n = int(b[i])<<16 | int(b[i+1])<<8 | int(b[i+2])
			out = append(out, alphabet[(n>>18)&0x3f], alphabet[(n>>12)&0x3f], alphabet[(n>>6)&0x3f], alphabet[n&0x3f])
		case i+1 < len(b):
			n = int(b[i])<<16 | int(b[i+1])<<8
			out = append(out, alphabet[(n>>18)&0x3f], alphabet[(n>>12)&0x3f], alphabet[(n>>6)&0x3f], '=')
		default:
			n = int(b[i]) << 16
			out = append(out, alphabet[(n>>18)&0x3f], alphabet[(n>>12)&0x3f], '=', '=')
		}
	}
	return string(out)
}
