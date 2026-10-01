// Package nativesessions owns read-only Claude and Codex transcript facts.
package nativesessions

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Header holds native identity and cwd facts, without import policy.
type Header struct {
	NativeID, Directory string
	StartedAt           time.Time
	IdentityMismatch    bool
}

// Inspect applies native header rules to a caller-owned bounded record scan.
func Inspect(harness, path string, scan func(func([]byte) bool) error) (Header, error) {
	var t Header
	if harness == "claude" {
		t.NativeID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	var err error
	switch harness {
	case "claude":
		err = scan(func(line []byte) bool {
			var r struct {
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(line, &r) == nil && r.Cwd != "" {
				t.Directory = r.Cwd
				return false
			}
			return true
		})
	case "codex":
		seen := 0
		metaFound := false
		err = scan(func(line []byte) bool {
			seen++
			var r struct {
				Type      string `json:"type"`
				Timestamp string `json:"timestamp"`
				Payload   struct {
					ID        string `json:"id"`
					SessionID string `json:"session_id"`
					Timestamp string `json:"timestamp"`
					Cwd       string `json:"cwd"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &r) != nil || r.Type != "session_meta" {
				return seen < codexMetaScanLimit
			}
			metaFound = true
			t.NativeID = r.Payload.ID
			t.Directory = r.Payload.Cwd
			fileID := rolloutFileID(filepath.Base(path))
			if r.Payload.ID == "" || (r.Payload.SessionID != "" && r.Payload.SessionID != r.Payload.ID) || fileID == "" || !strings.EqualFold(fileID, r.Payload.ID) {
				t.IdentityMismatch = true
			}
			for _, ts := range []string{r.Payload.Timestamp, r.Timestamp} {
				if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
					t.StartedAt = parsed.UTC()
					break
				}
			}
			return false
		})
		if !metaFound {
			// Without session_meta there is no ID to register the session
			// under, so it cannot be matched with a hook registration.
			t.IdentityMismatch = true
		}

	case "cursor":
		// Cursor's ID and project come from its folders, not its records.
	}
	return t, err
}

// codexMetaScanLimit bounds how far into a Codex rollout session_meta is
// looked for. Codex writes it first.
const codexMetaScanLimit = 16

// rolloutUUID is the session UUID at the end of a Codex rollout file name.
var rolloutUUID = regexp.MustCompile(`([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.jsonl$`)

// rolloutFileID returns the UUID a rollout file is named with, or "".
func rolloutFileID(name string) string {
	if m := rolloutUUID.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
}
