package retention

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type protectedExpiryMode string

const (
	protectedExpiryCorrupt    protectedExpiryMode = "corrupt"
	protectedExpiryFuture     protectedExpiryMode = "future"
	protectedExpirySourceOnly protectedExpiryMode = "source-only"
	protectedExpiryOpaque     protectedExpiryMode = "opaque"
)

func protectedExpiryFixture(t *testing.T, mode protectedExpiryMode, registered bool) (*state.Store, archive.SessionRegistration, map[string][]byte) {
	t.Helper()
	local := newTestStore(t)
	if err := config.WithDurableStorage(local.Home(), func(config.DurableStorageGuard) error { return nil }); err != nil {
		t.Fatal(err)
	}
	reg := registration("protected", filepath.Join(t.TempDir(), "missing.jsonl"))
	if registered {
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	if err := local.RecordSuperseded(reg.ArchiveSessionID, "sessions/codex/protected/sources/original.json", reg.RegisteredAt); err != nil {
		t.Fatal(err)
	}
	candidate := state.SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "native-child", ParentArchiveSessionID: reg.ArchiveSessionID, ParentNativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, ProjectRoot: reg.ProjectRoot, Harness: reg.Harness, AgentID: "child", TranscriptPath: "/synthetic/missing-child", ObservedAt: reg.RegisteredAt}
	if err := local.SaveSubagentCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(local.Home(), "pending", reg.ArchiveSessionID+".json")
	raw := []byte("{original")
	switch mode {
	case protectedExpiryFuture:
		raw = []byte(`{"history":{"version":2}}`)
	case protectedExpirySourceOnly:
		sum := sha256.Sum256(raw)
		stage, err := local.StagePendingSource(reg.ArchiveSessionID, archive.SourceReference{Key: "synthetic", SHA256: hex.EncodeToString(sum[:]), CompressedBytes: len(raw)}, raw)
		if err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(local.SessionDir(reg.ArchiveSessionID), "pending-sources", stage.Name)
	case protectedExpiryOpaque:
		path = filepath.Join(local.Home(), "publication-evidence", reg.ArchiveSessionID, "opaque")
	case protectedExpiryCorrupt:
	}
	if mode != protectedExpirySourceOnly {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	originals := map[string][]byte{}
	paths := []string{path, filepath.Join(local.Home(), "superseded", reg.ArchiveSessionID+".json"), filepath.Join(local.Home(), "subagent-candidates", "child.json")}
	if registered {
		paths = append(paths, filepath.Join(local.Home(), "registrations", reg.ArchiveSessionID+".json"))
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		originals[p] = raw
	}
	return local, reg, originals
}

func checkProtectedExpiryBytes(t *testing.T, originals map[string][]byte) {
	t.Helper()
	for p, raw := range originals {
		after, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(after, raw) {
			t.Errorf("protected original changed %s: %v", p, err)
		}
	}
}

func TestRetentionRefusesProtectedRecoveryBeforeAnyDelete(t *testing.T) {
	for _, mode := range []protectedExpiryMode{protectedExpiryCorrupt, protectedExpiryFuture, protectedExpirySourceOnly, protectedExpiryOpaque} {
		t.Run(string(mode), func(t *testing.T) {
			for _, registered := range []bool{false, true} {
				t.Run(map[bool]string{false: "orphan", true: "excluded-registered"}[registered], func(t *testing.T) {
					local, reg, originals := protectedExpiryFixture(t, mode, registered)
					remote := &deleteRecordingStore{MemoryStore: storagetest.NewMemoryStore()}
					sourceKey := "sessions/codex/protected/sources/original.json"
					if err := remote.Put(t.Context(), sourceKey, []byte("remote-original")); err != nil {
						t.Fatal(err)
					}
					now := time.Now().Add(retentionWindow + time.Hour)
					result, err := Sweep(t.Context(), local, remote, agreeing(Options{Now: func() time.Time { return now }, SessionMaxAge: retentionWindow, Publishable: func(archive.SessionRegistration) bool { return false }}))
					if err != nil || !errors.Is(result.Errors[reg.ArchiveSessionID], state.ErrDurableStorageRecovery) || len(remote.deleted) != 0 || len(result.DeletedSessions)+len(result.PrunedSessions) != 0 {
						t.Errorf("protected cleanup escaped: result=%+v deletes=%v err=%v", result, remote.deleted, err)
					}
					if got, err := remote.Get(t.Context(), sourceKey); err != nil || string(got) != "remote-original" {
						t.Errorf("remote original changed: %v", err)
					}
					checkProtectedExpiryBytes(t, originals)
				})
			}
		})
	}
}

type protectedForgetRoute string

const (
	protectedForgetOrphan protectedForgetRoute = "orphan"
	protectedForgetIdle   protectedForgetRoute = "idle"
	protectedForgetDirect protectedForgetRoute = "direct"
)

func TestSharedForgettingRefusesBeforeProtectedRecordsChange(t *testing.T) {
	for _, mode := range []protectedExpiryMode{protectedExpiryCorrupt, protectedExpiryFuture, protectedExpirySourceOnly, protectedExpiryOpaque} {
		t.Run(string(mode), func(t *testing.T) {
			for _, route := range []protectedForgetRoute{protectedForgetOrphan, protectedForgetIdle, protectedForgetDirect} {
				t.Run(string(route), func(t *testing.T) {
					local, reg, originals := protectedExpiryFixture(t, mode, route != protectedForgetOrphan)
					key := agentmeta.SessionKey{Agent: agentmeta.ID(reg.Harness.Name), NativeID: reg.NativeSessionID}
					var err error
					var forgotten bool
					switch route {
					case protectedForgetIdle:
						forgotten, err = local.ForgetIdleSession(reg.ArchiveSessionID, key, false, &state.RemovalRecord{Harness: reg.Harness.Name, Reason: state.RemovalReasonRetention, At: time.Now()})
					case protectedForgetDirect:
						err = local.ForgetSession(reg.ArchiveSessionID, key)
					case protectedForgetOrphan:
						forgotten, err = local.ForgetOrphan(reg.ArchiveSessionID)
					}
					if forgotten || !errors.Is(err, state.ErrDurableStorageRecovery) {
						t.Errorf("shared forget allowed: %t %v", forgotten, err)
					}
					checkProtectedExpiryBytes(t, originals)
				})
			}
		})
	}
}

type protectedDeleteLane string

const (
	protectedDeleteWhole      protectedDeleteLane = "whole-session"
	protectedDeleteSuperseded protectedDeleteLane = "superseded"
)

type protectedMetadataReadStore struct {
	*deleteRecordingStore
	beforeRead func() error
}

func (s *protectedMetadataReadStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if s.beforeRead != nil {
		change := s.beforeRead
		s.beforeRead = nil
		if err := change(); err != nil {
			return nil, err
		}
	}
	return s.MemoryStore.GetLimited(ctx, key, limit)
}

func TestRetentionRechecksProtectedRecoveryAfterMetadataRead(t *testing.T) {
	for _, lane := range []protectedDeleteLane{protectedDeleteWhole, protectedDeleteSuperseded} {
		t.Run(string(lane), func(t *testing.T) {
			local := newTestStore(t)
			remote := &protectedMetadataReadStore{deleteRecordingStore: &deleteRecordingStore{MemoryStore: storagetest.NewMemoryStore()}}
			dir := t.TempDir()
			t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
			publishTwice(t, local, remote, "protected", dir, t0)
			if lane == protectedDeleteSuperseded {
				publishThird(t, local, remote, "protected", dir, t0.Add(time.Hour))
			}
			objects, err := remote.List(t.Context(), "sessions/codex/protected/")
			if err != nil {
				t.Fatal(err)
			}
			originals := map[string][]byte{}
			for _, object := range objects {
				data, err := remote.Get(t.Context(), object.Key)
				if err != nil {
					t.Fatal(err)
				}
				originals[object.Key] = data
			}
			localOriginals := map[string][]byte{}
			for _, dir := range []string{"registrations", "published", "superseded"} {
				path := filepath.Join(local.Home(), dir, "protected.json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				localOriginals[path] = data
			}
			pendingPath := filepath.Join(local.Home(), "pending", "protected.json")
			pending := []byte(`{"history":{"version":2}}`)
			remote.deleted = nil
			remote.beforeRead = func() error { return os.WriteFile(pendingPath, pending, 0600) }
			now := t0.Add(retentionWindow + time.Hour)
			var maxAge time.Duration
			if lane == protectedDeleteWhole {
				maxAge = retentionWindow
			}
			opts := Options{Now: func() time.Time { return now }, SessionMaxAge: maxAge}
			result, err := Sweep(t.Context(), local, remote, agreeing(opts))
			if err != nil || !errors.Is(result.Errors["protected"], state.ErrDurableStorageRecovery) || len(remote.deleted) != 0 {
				t.Errorf("entry observation authorized late delete: result=%+v deletes=%v err=%v", result, remote.deleted, err)
			}
			for key, original := range originals {
				data, err := remote.Get(t.Context(), key)
				if err != nil || !bytes.Equal(data, original) {
					t.Errorf("remote original changed %s: %v", key, err)
				}
			}
			localOriginals[pendingPath] = pending
			checkProtectedExpiryBytes(t, localOriginals)
		})
	}
}
