package collector

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

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type recoveryLabelLookup struct {
	provider     *mutableLabels
	acquisitions int
}

func (l *recoveryLabelLookup) LookupLabels(string) (agentapi.LabelProvider, bool) {
	l.acquisitions++
	return mutableLabelProvider{l.provider}, true
}

type recoveryNativeLookup struct {
	provider     codex.NativeLabelProvider
	acquisitions int
}

func (l *recoveryNativeLookup) LookupLabels(string) (agentapi.LabelProvider, bool) {
	l.acquisitions++
	return l.provider, true
}

type recoveryNativeTransport struct{ writes int }

func (h *recoveryNativeTransport) WriteLine(context.Context, []byte) error {
	h.writes++
	return agentapi.ErrLabelHostUnavailable
}

func (*recoveryNativeTransport) ReadLine(context.Context) ([]byte, error) {
	return nil, agentapi.ErrLabelHostUnavailable
}

func (*recoveryNativeTransport) Close() error { return nil }

func TestRegisteredRecoveryRefusesNativeNamingBeforeProvider(t *testing.T) {
	for _, mode := range []durableSessionMode{durableSessionModeOpaque, durableSessionModeSourceOnly, durableSessionModeCorruptPending} {
		t.Run(string(mode), func(t *testing.T) {
			for _, warm := range []bool{false, true} {
				name := "cold"
				if warm {
					name = "due-warm"
				}
				t.Run(name, func(t *testing.T) {
					s := newTestStore(t)
					nativeHome := t.TempDir()
					if err := os.Mkdir(filepath.Join(nativeHome, "sessions"), 0700); err != nil {
						t.Fatal(err)
					}
					path := writeTranscript(t, filepath.Join(nativeHome, "sessions"), "session.jsonl", `{"type":"session_meta","payload":{"id":"01900000-0000-7000-8000-000000000001","cli_version":"0.159.2","history_mode":"legacy"}}`+"\n"+codexTranscript)
					reg := registration(t, path)
					reg.NativeSessionID = "01900000-0000-7000-8000-000000000001"
					reg.Harness.Version = "0.159.2"
					if err := s.SaveRegistration(reg); err != nil {
						t.Fatal(err)
					}
					remote := storagetest.NewMemoryStore()
					now := reg.RegisteredAt.Add(time.Hour)
					provider := &mutableLabels{label: archive.SessionLabel{State: archive.SessionLabelPresent, Name: "Retained name", Source: archive.SessionLabelDatabase, Contract: "synthetic"}}
					lookup := &recoveryLabelLookup{provider: provider}
					filter := &operationFilter{}
					bindings := &operationBindings{parser: &operationParser{version: "0.1.0"}, filter: filter}
					opts := Options{MachineID: "machine", Sources: bindings, Parsers: bindings, Now: func() time.Time { return now }}
					if result, err := Run(t.Context(), s, remote, opts); err != nil || len(result.Errors) != 0 {
						t.Fatal(result, err)
					}
					host := &recoveryNativeTransport{}
					starts := 0
					native := &recoveryNativeLookup{provider: codex.NativeLabelProvider{Host: func(context.Context, string) (agentapi.LabelTransport, error) {
						starts++
						return host, nil
					}}}
					nativeOpts := opts
					nativeOpts.Labels = native
					nativeOpts.LabelEnvironment = agentapi.LabelEnvironment{Mode: agentapi.LabelLookupNative, Homes: []string{nativeHome}, ProviderContract: codex.LabelAPIContract}
					now = now.Add(time.Hour)
					if result, err := Run(t.Context(), s, remote, nativeOpts); err != nil || len(result.Errors) != 0 || starts != 1 || host.writes != 1 {
						t.Fatal("healthy native host disabled", result, err, starts, host.writes)
					}
					opts.Labels = lookup
					if warm {
						now = now.Add(time.Hour)
						if result, err := Run(t.Context(), s, remote, opts); err != nil || len(result.Errors) != 0 || provider.calls != 1 {
							t.Fatal("healthy naming disabled", result, err, provider.calls)
						}
					}
					publishedPath := filepath.Join(s.Home(), "published", reg.ArchiveSessionID+".json")
					published, err := os.ReadFile(publishedPath)
					if err != nil {
						t.Fatal(err)
					}
					var owedPath string
					switch mode {
					case durableSessionModeOpaque:
						owedPath = filepath.Join(s.Home(), "publication-evidence", reg.ArchiveSessionID, "opaque")
					case durableSessionModeSourceOnly:
						owedPath = ""
					case durableSessionModeCorruptPending:
						owedPath = filepath.Join(s.Home(), "pending", reg.ArchiveSessionID+".json")
					case durableSessionModeMissingPending, durableSessionModeMissingSource, durableSessionModeCanceled:
						t.Fatalf("unexpected mode outside registered recovery fixture: %q", mode)
					}
					owed := []byte("{sole-original")
					if mode == durableSessionModeSourceOnly {
						sum := sha256.Sum256(owed)
						stage, err := s.StagePendingSource(reg.ArchiveSessionID, archive.SourceReference{SHA256: hex.EncodeToString(sum[:]), Key: "synthetic", CompressedBytes: len(owed)}, owed)
						if err != nil {
							t.Fatal(err)
						}
						owedPath = filepath.Join(s.Home(), "sessions", reg.ArchiveSessionID, "pending-sources", stage.Name)
					} else {
						if err := os.MkdirAll(filepath.Dir(owedPath), 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(owedPath, owed, 0600); err != nil {
							t.Fatal(err)
						}
					}
					provider.calls, lookup.acquisitions = 0, 0
					provider.label.Name = "Forbidden new observation"
					now = now.Add(time.Hour)
					filter.calls = 0
					result, err := Run(t.Context(), s, remote, opts)
					if !errors.Is(err, state.ErrDurableStorageRecovery) && !errors.Is(result.Errors[reg.ArchiveSessionID], state.ErrDurableStorageRecovery) {
						t.Fatalf("missing recovery: %+v %v", result, err)
					}
					if provider.calls != 0 || lookup.acquisitions != 0 || filter.calls != 0 || len(result.Published) != 0 {
						t.Errorf("recovery reached naming: native=%d acquisition=%d filter=%d published=%v", provider.calls, lookup.acquisitions, filter.calls, result.Published)
					}
					cache, err := s.LoadLabels()
					if err != nil {
						t.Fatal(err)
					}
					if entry, ok := cache.Entries[reg.ArchiveSessionID]; ok && entry.Label.Name == provider.label.Name {
						t.Error("adopted new observation over recovery")
					}
					starts, host.writes, native.acquisitions = 0, 0, 0
					nativeResult, nativeErr := Run(t.Context(), s, remote, nativeOpts)
					if !errors.Is(nativeErr, state.ErrDurableStorageRecovery) && !errors.Is(nativeResult.Errors[reg.ArchiveSessionID], state.ErrDurableStorageRecovery) {
						t.Fatal("native recovery absent", nativeResult, nativeErr)
					}
					if starts != 0 || host.writes != 0 || native.acquisitions != 0 || filter.calls != 0 {
						t.Errorf("recovery reached native host: starts=%d writes=%d acquisition=%d filter=%d", starts, host.writes, native.acquisitions, filter.calls)
					}
					for p, want := range map[string][]byte{owedPath: owed, publishedPath: published} {
						got, err := os.ReadFile(p)
						if err != nil || !bytes.Equal(got, want) {
							t.Fatal("protected bytes changed", p, err)
						}
					}
				})
			}
		})
	}
}

func TestHealthyPendingNamingEligibilityReleasesScratch(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return reg.RegisteredAt.Add(time.Hour) }}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || len(result.Errors) == 0 {
		t.Fatal("expected attempted pending", result, err)
	}
	pending, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || !pending.Attempted {
		t.Fatal("invalid pending control", found, err)
	}
	closeSources := openCursorPass([]archive.SessionRegistration{reg}, &opts)
	defer func() {
		if err := closeSources(); err != nil {
			t.Error(err)
		}
	}()
	ledger := opts.sourcePasses.env.ReadBudget
	scoped, closeScope := local.WithReadBudget(t.Context(), ledger)
	p := &pass{ctx: t.Context(), local: local, opts: opts, result: Result{Errors: map[string]error{}}, unreadable: map[string]bool{}, labelLocal: scoped, closeLabelResources: closeScope}
	defer p.releaseLabelResources()
	before := ledger.Available()
	for range 3 {
		if (&sessionScan{opts: p.opts}).readBudget() != ledger {
			t.Fatal("guard received a different ledger")
		}
		if !p.labelEligible(reg) {
			t.Fatal("healthy pending ineligible", p.result.Errors)
		}
		if ledger.Available() != before {
			t.Errorf("discarded eligibility input retained: before=%d after=%d", before, ledger.Available())
		}
	}
}
