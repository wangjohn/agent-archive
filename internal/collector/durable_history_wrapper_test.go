package collector

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type cycle3HistoryWrapperMode string

const (
	cycle3WrapperOrdinary        cycle3HistoryWrapperMode = "ordinary-source2-metadata1"
	cycle3WrapperSourceVersion   cycle3HistoryWrapperMode = "source-history-schema3"
	cycle3WrapperSourceHistory   cycle3HistoryWrapperMode = "source-history-pointer"
	cycle3WrapperMetadataVersion cycle3HistoryWrapperMode = "metadata-history-schema2"
	cycle3WrapperMetadataHistory cycle3HistoryWrapperMode = "metadata-history-pointer"
)

func TestProtectedHistoryWrapperUsesOrdinaryPublicationFence(t *testing.T) {
	for _, mode := range []cycle3HistoryWrapperMode{cycle3WrapperOrdinary, cycle3WrapperSourceVersion, cycle3WrapperSourceHistory, cycle3WrapperMetadataVersion, cycle3WrapperMetadataHistory} {
		t.Run(string(mode), func(t *testing.T) {
			local := newTestStore(t)
			reg := registration(t, writeTranscript(t, t.TempDir(), "diagnostic.jsonl", codexTranscript))
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			source := []byte("correct-checksum diagnostic source")
			sum := sha256.Sum256(source)
			bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion}
			metadata := archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion}
			switch mode {
			case cycle3WrapperOrdinary:
			case cycle3WrapperSourceVersion:
				bundle.SchemaVersion = archive.HistorySourceSchemaVersion
			case cycle3WrapperSourceHistory:
				bundle.History = &archive.SourceHistory{}
			case cycle3WrapperMetadataVersion:
				metadata.SchemaVersion = archive.HistoryMetadataSchemaVersion
			case cycle3WrapperMetadataHistory:
				metadata.History = &archive.RevisionHistory{}
			}
			rawMetadata, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			pending := state.PendingPublication{Bundle: bundle, SourceKey: "source", MetadataKey: "metadata", SourceSHA256: hex.EncodeToString(sum[:]), SourceBytes: source, MetadataBytes: rawMetadata, Attempted: true}
			if err := local.SavePending(reg.ArchiveSessionID, pending); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(local.Home(), "pending", reg.ArchiveSessionID+".json")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			budget := agentapi.NewNativeReadBudget(128 << 20)
			scoped, closeScope := local.WithReadBudget(t.Context(), budget)
			before := budget.Available()
			guardErr := scoped.CheckDurableSessionRead(reg.ArchiveSessionID)
			afterGuard := budget.Available()
			_, found, loadErr := scoped.LoadPending(reg.ArchiveSessionID)
			scan := sessionScan{ctx: t.Context(), reg: reg, remote: storagetest.NewMemoryStore(), retainedBudget: budget}
			fenceErr := scan.checkHistoryPublication(pending)
			closeScope()
			if afterGuard != before || budget.Available() != before {
				t.Fatalf("shared ledger changed: before=%d guard=%d final=%d", before, afterGuard, budget.Available())
			}
			filter := &operationFilter{}
			bindings := &operationBindings{parser: &operationParser{version: "0.1.0"}, filter: filter}
			preview, previewErr := ReadLocalBundle(t.Context(), local.Home(), reg, time.Now(), "", bindings)
			preserved, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(original, preserved) {
				t.Fatal("named pending changed", err)
			}
			t.Logf("nil wrapper History=%t LoadPending(found=%t err=%v) guard=%v ordinaryFence=%v preview(filter=%d emitted=%s err=%v) sharedBudgetUnchanged=%t bytesRetained=%d", pending.History == nil, found, loadErr, guardErr, fenceErr, filter.calls, preview.ArchiveSessionID, previewErr, budget.Available() == before, len(preserved))
			if mode == cycle3WrapperOrdinary {
				if guardErr != nil || loadErr != nil || !found || fenceErr != nil || filter.calls != 1 || previewErr != nil {
					t.Fatal("healthy ordinary control refused")
				}
			} else {
				if !errors.Is(fenceErr, archive.ErrHistoryMutationPending) {
					t.Fatal("not an actual ordinary publication fence trigger", fenceErr)
				}
				if !found || !errors.Is(loadErr, state.ErrDurableStorageRecovery) || !errors.Is(guardErr, state.ErrDurableStorageRecovery) || !errors.Is(previewErr, state.ErrDurableStorageRecovery) || filter.calls != 0 {
					t.Errorf("ordinary history-shaped wrapper grants pre-publication content access")
				}
			}
		})
	}
}

func TestProtectedSupportedNonNilHistoryRead(t *testing.T) {
	scan, pending, _, _ := frozenHistoryFixture(t)
	if pending.History == nil {
		t.Fatal("control is not the supported history lane")
	}
	budget := agentapi.NewNativeReadBudget(128 << 20)
	scoped, closeScope := scan.local.WithReadBudget(t.Context(), budget)
	before := budget.Available()
	guardErr := scoped.CheckDurableSessionRead(scan.reg.ArchiveSessionID)
	got, found, loadErr := scoped.LoadPending(scan.reg.ArchiveSessionID)
	supported := loadErr == nil && found && got.History != nil && got.History.Version == 1
	scan.retainedBudget = budget
	fenceErr := scan.checkHistoryPublication(got)
	closeScope()
	if guardErr != nil || !supported || fenceErr != nil || budget.Available() != before {
		t.Fatalf("supported non-nil history refused/leaked: guard=%v read=%v found=%t budget=%d/%d", guardErr, loadErr, found, budget.Available(), before)
	}
	t.Logf("actual frozenHistoryFixture non-nil PendingHistory1 remains readable, guard-supported and actual history-publication fence accepted=%t; no native naming authority inferred", fenceErr == nil)
}
