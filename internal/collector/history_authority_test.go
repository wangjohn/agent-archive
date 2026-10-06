package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestOrdinaryPrivacyMaintenanceCannotCertifyHistoryAuthority(t *testing.T) {
	t.Parallel()
	local, cloud := newTestStore(t), storagetest.NewMemoryStore()
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishCodexSession(t, local, cloud, at)
	simulateFilterUpgrade(t, local)
	reg, _, err := local.LoadRegistration("session-1")
	if err != nil {
		t.Fatal(err)
	}
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	scan := newSessionScan(t.Context(), local, cloud, reg, state.Request{}, published, at.Add(time.Hour), Options{MachineID: "m"})
	if recover, err := scan.requiresReferenceRecovery(); err != nil || recover {
		t.Fatalf("ordinary privacy maintenance lost its retained input: %v %v", recover, err)
	}
	if _, _, err := published.LastPublishedMetadata(); err == nil {
		t.Fatal("ordinary maintenance certified mismatched history authority")
	}
	var original archive.Metadata
	if err := json.Unmarshal(published.Metadata(), &original); err != nil {
		t.Fatal(err)
	}
	for _, damage := range []string{"schema", "history", "native", "project", "harness", "parent", "capture", "reference", "owner"} {
		t.Run(damage, func(t *testing.T) {
			metadata := original
			switch damage {
			case "schema":
				metadata.SchemaVersion = 99
			case "history":
				metadata.History = &archive.RevisionHistory{}
			case "native":
				metadata.NativeSessionID = "another-thread"
			case "project":
				metadata.ProjectID = "another-project"
			case "harness":
				metadata.Harness.Name = "claude-code"
			case "parent":
				metadata.ParentSessionID = "another-parent"
			case "capture":
				metadata.CapturedAt = at.Add(time.Minute)
			case "reference":
				metadata.SourceBundle.SHA256 = "another-checksum"
			case "owner":
				metadata.MachineID = "another-machine"
			}
			raw, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err := published.CacheMetadata(raw); err != nil {
				t.Fatal(err)
			}
			if scan.ordinaryPrivacyMaintenance() {
				t.Fatal("uncertain metadata entered ordinary maintenance")
			}
		})
	}
}

func TestRunRecoversWholeHistoryAuthorityAfterStateLoss(t *testing.T) {
	t.Parallel()
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "corrupt"}[corrupt], func(t *testing.T) {
			t.Parallel()
			scan, pending, cloud, _ := frozenHistoryFixture(t)
			if err := scan.local.SaveRegistration(scan.reg); err != nil {
				t.Fatal(err)
			}
			if err := scan.local.RemovePending(scan.id()); err != nil {
				t.Fatal(err)
			}
			if err := cloud.Put(t.Context(), pending.MetadataKey, pending.MetadataBytes); err != nil {
				t.Fatal(err)
			}
			if corrupt {
				path := filepath.Join(scan.local.Home(), "published", scan.id()+".json")
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			opts := Options{MachineID: "m", Sources: testSources, Parsers: testParsers, Now: func() time.Time { return scan.now }}
			if corrupt {
				if _, err := Run(t.Context(), scan.local, cloud, opts); err != nil {
					t.Fatal(err)
				}
			}
			result, err := Run(t.Context(), scan.local, cloud, opts)
			if err != nil || !errors.Is(result.Errors[scan.id()], archive.ErrHistoryMutationPending) {
				t.Fatalf("%#v %v", result, err)
			}
			published, err := scan.local.LoadPublishedState(scan.id())
			if err != nil {
				t.Fatal(err)
			}
			metadata, found, err := published.LastPublishedMetadata()
			if err != nil || !found || len(metadata.History.Preserved) != 1 || !bytes.Equal(published.Metadata(), pending.MetadataBytes) {
				t.Fatalf("incomplete authority: %#v %v %v", metadata, found, err)
			}
			raw, err := cloud.Get(t.Context(), pending.MetadataKey)
			if err != nil || !bytes.Equal(raw, pending.MetadataBytes) {
				t.Fatal("remote sidecar changed", err)
			}
		})
	}
}

func TestRunRefusesUncertainRemoteAuthorityWithoutCachingOrMutation(t *testing.T) {
	t.Parallel()
	for _, damage := range []string{"missing", "checksum", "wrong_thread", "future_schema", "foreign_owner", "filter_provenance", "format_provenance"} {
		t.Run(damage, func(t *testing.T) {
			t.Parallel()
			scan, pending, cloud, previous := frozenHistoryFixture(t)
			if err := scan.local.SaveRegistration(scan.reg); err != nil {
				t.Fatal(err)
			}
			if err := scan.local.RemovePending(scan.id()); err != nil {
				t.Fatal(err)
			}
			var metadata archive.Metadata
			if err := json.Unmarshal(pending.MetadataBytes, &metadata); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "missing":
				if err := cloud.Delete(t.Context(), previous.Key); err != nil {
					t.Fatal(err)
				}
			case "checksum":
				if err := cloud.Put(t.Context(), previous.Key, []byte("corrupt")); err != nil {
					t.Fatal(err)
				}
			case "wrong_thread":
				foreign := pending.Bundle
				foreign.NativeSessionID = "33333333-3333-4333-8333-333333333333"
				foreign.Capture.CapturedAt = metadata.History.Preserved[0].CapturedAt
				packed, err := archive.BuildCompressedSource(foreign)
				if err != nil {
					t.Fatal(err)
				}
				key, err := archive.SourceObjectKey(foreign, packed.SHA256)
				if err != nil {
					t.Fatal(err)
				}
				if err := cloud.Put(t.Context(), key, packed.Bytes); err != nil {
					t.Fatal(err)
				}
				metadata.History.Preserved[0].Source = archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
			case "future_schema":
				metadata.SchemaVersion = 99
				metadata.History = nil
			case "foreign_owner":
				metadata.MachineID = "another"
			case "filter_provenance":
				metadata.History.Preserved[0].FilterVersion = "different"
			case "format_provenance":
				metadata.History.Preserved[0].SourceSchemaVersion = 3
			}
			raw, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err := cloud.Put(t.Context(), pending.MetadataKey, raw); err != nil {
				t.Fatal(err)
			}
			result, err := Run(t.Context(), scan.local, cloud, Options{MachineID: "m", Sources: testSources, Parsers: testParsers, Now: func() time.Time { return scan.now }})
			if err != nil || result.Errors[scan.id()] == nil {
				t.Fatalf("uncertain authority accepted: %#v %v", result, err)
			}
			published, err := scan.local.LoadPublishedState(scan.id())
			if err != nil || published.Found() {
				t.Fatal("cached partial authority", err)
			}
			unchanged, err := cloud.Get(t.Context(), pending.MetadataKey)
			if err != nil || !bytes.Equal(raw, unchanged) {
				t.Fatal("uncertain remote mutated", err)
			}
		})
	}
}

func TestAuthorityRecoveryPreservesNewerLocalCandidate(t *testing.T) {
	t.Parallel()
	scan, pending, cloud, _ := frozenHistoryFixture(t)
	if err := cloud.Put(t.Context(), pending.MetadataKey, pending.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	published, err := scan.local.LoadPublishedState(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	newer := pending.Bundle
	newer.Capture.CapturedAt = scan.now.Add(time.Hour)
	newer.NativeRecords = append(newer.NativeRecords, map[string]any{"type": "synthetic_new"})
	if err := published.Save(newer, scan.now, state.CacheStatusRateLimited); err != nil {
		t.Fatal(err)
	}
	scan.published = published
	if err := scan.recoverReferenceAuthority(); err != nil {
		t.Fatal(err)
	}
	cached, _, status, _ := published.Cached()
	if status != state.CacheStatusRateLimited || len(cached.NativeRecords) != 2 || !cached.Capture.CapturedAt.Equal(newer.Capture.CapturedAt) {
		t.Fatal("newer evidence lost")
	}
	if metadata, found, err := published.LastPublishedMetadata(); err != nil || !found || len(metadata.History.Preserved) != 1 {
		t.Fatalf("acknowledged set missing: %v %v", found, err)
	}
}
