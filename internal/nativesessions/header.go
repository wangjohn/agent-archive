// Package nativesessions owns read-only Claude and Codex transcript facts.
package nativesessions

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
)

// Header holds native identity and cwd facts, without import policy.
type Header struct {
	NativeID         string
	Directory        string
	StartedAt        time.Time
	IdentityMismatch bool
}

type harnessName string

const (
	harnessClaude harnessName = "claude"
	harnessCodex  harnessName = "codex"
	harnessCursor harnessName = "cursor"
)

// Inspect applies native header rules to a caller-owned bounded record scan.
func Inspect(harness, path string, scan func(func([]byte) bool) error) (Header, error) {
	var nativeID string
	if harnessName(harness) == harnessClaude {
		nativeID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	t := Header{NativeID: nativeID}
	var err error
	switch harnessName(harness) {
	case harnessClaude:
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
	case harnessCodex:
		seen := 0
		metaFound := false
		err = scan(func(line []byte) bool {
			seen++
			meta, start, found, _ := ParseCodexMeta(line)
			if !found {
				return seen < codexMetaScanLimit
			}
			metaFound = true
			t.NativeID, t.Directory, t.StartedAt = meta.ID, meta.Cwd, start
			fileID := RolloutID(path)
			t.IdentityMismatch = meta.ID == "" || (meta.SessionID != "" && meta.SessionID != meta.ID) || fileID == "" || !strings.EqualFold(fileID, meta.ID)
			return false
		})
		if !metaFound {
			// Without session_meta there is no ID to register the session
			// under, so it cannot be matched with a hook registration.
			t.IdentityMismatch = true
		}

	case harnessCursor:
		// Cursor's ID and project come from its folders, not its records.
	}
	return t, err
}

// codexMetaScanLimit bounds how far into a Codex rollout session_meta is
// looked for. Codex writes it first.
const codexMetaScanLimit = 16
