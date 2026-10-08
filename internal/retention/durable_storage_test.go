package retention

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRetentionUnknownPublishedPrefixCannotDelete(t *testing.T) {
	for _, mode := range []string{"missing", "future-header"} {
		t.Run(mode, func(t *testing.T) {
			local := newTestStore(t)
			reg := registration("protected", filepath.Join(t.TempDir(), "missing.jsonl"))
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			raw := []byte(`{"summary":{"published":true,"harness":"codex","captured_at":"2026-01-01T00:00:00Z"}}`)
			if mode == "missing" {
				if err := os.Remove(filepath.Join(local.Home(), "config.json")); err != nil {
					t.Fatal(err)
				}
			} else {
				raw = []byte(`{"publication_version":2,"summary":{"published":true}}`)
			}
			p := filepath.Join(local.Home(), "published", "protected.json")
			if err := os.WriteFile(p, raw, 0600); err != nil {
				t.Fatal(err)
			}
			remote := &deleteRecordingStore{MemoryStore: storagetest.NewMemoryStore()}
			result := sweep(t, local, remote, reg.RegisteredAt.Add(retentionWindow+time.Hour), Options{})
			if !errors.Is(result.Errors["protected"], state.ErrDurableStorageRecovery) || len(remote.deleted) != 0 || len(result.DeletedSessions)+len(result.PrunedSessions) != 0 {
				t.Fatalf("prefix granted deletion: %+v %+v", result, remote.deleted)
			}
			if _, found, err := local.LoadRegistration("protected"); err != nil || !found {
				t.Fatal("registration forgotten", err)
			}
			after, err := os.ReadFile(p)
			if err != nil || string(after) != string(raw) {
				t.Fatal("original state removed", err)
			}
		})
	}
}
