package capture

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"os"
	"testing"
	"time"
)

func TestThreadTriageFollowupThenStart(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	makeBatch := func(project string) []agentapi.LifecycleEvent {
		follow := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalAnswer, "triage", project, "/synthetic/source"), ObservedAt: at})
		start := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalBegin, "triage", project, ""), ObservedAt: at})
		return append(follow, start...)
	}
	direct, directProject := t.TempDir(), t.TempDir()
	setUpTestConfig(t, direct, directProject, at.Add(-time.Hour))
	if err := HandleBatch(direct, "synthetic", makeBatch(directProject), at); err != nil {
		t.Fatal(err)
	}
	regs, err := state.OpenReadOnly(direct).LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("direct control registrations=%d err=%v", len(regs), err)
	}
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	batch, err := validateBatch("synthetic", makeBatch(project), at)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadHookCaptureWindow(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := queueEventBatchInGeneration(home, batch, at, cfg.PauseGeneration, nil)
	if err != nil || !queued {
		t.Fatalf("queue=%v err=%v", queued, err)
	}
	for i := 1; i <= 2; i++ {
		if err := ReplayAdmissionIntents(home, at.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	regs, err = state.OpenReadOnly(home).LoadRegistrations()
	entries, queueErr := os.ReadDir(admissionIntentDir(home))
	if err != nil || len(regs) != 1 {
		t.Fatalf("direct control registered; replay registrations=%d err=%v remaining intents=%d queueErr=%v", len(regs), err, len(entries), queueErr)
	}
}

func TestFollowupStartBatchSurvivesEachPartialReplay(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, boundary := range []effectName{effectRegistrationCreate, effectLocatorUpdate, effectEvidenceSave, effectRequestSave, effectIntentAck} {
		t.Run(string(boundary), func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			follow := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalAnswer, "ordered", project, "/earlier"), ObservedAt: at})
			start := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalBegin, "ordered", project, ""), ObservedAt: at})
			later := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalAnswer, "ordered", project, "/later"), ObservedAt: at})
			later[0].Reason = "later"
			later[0].NativeEvent = "later"
			batch := append(append(follow, start...), later...)
			batch, err := validateBatch("synthetic", batch, at)
			if err != nil {
				t.Fatal(err)
			}
			cfg, _, err := loadHookCaptureWindow(home, nil)
			if err != nil {
				t.Fatal(err)
			}
			queued, err := queueEventBatchInGeneration(home, batch, at, cfg.PauseGeneration, nil)
			if err != nil || !queued {
				t.Fatalf("queue %v %v", queued, err)
			}
			interrupted := false
			err = replayAdmissionIntents(home, at.Add(time.Second), nil, func(name effectName) error {
				if name == boundary && !interrupted {
					interrupted = true
					return errors.New("partial ordered batch")
				}
				return nil
			})
			if err == nil || !interrupted {
				t.Fatalf("missing actual failure %v", err)
			}
			entries, err := os.ReadDir(admissionIntentDir(home))
			if err != nil {
				t.Fatal(err)
			}
			if boundary != effectIntentAck && len(entries) != 1 {
				t.Fatalf("whole batch not retained: %v", entries)
			}
			for range 2 {
				if err := ReplayAdmissionIntents(home, at.Add(2*time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			regs, err := state.OpenReadOnly(home).LoadRegistrations()
			if err != nil || len(regs) != 1 {
				t.Fatalf("registrations %+v %v", regs, err)
			}
			reg := regs[0]
			if reg.TranscriptPath != "/earlier" || !reg.AdmittedAt.Equal(at) || reg.Origin != archive.SessionOriginHook {
				t.Fatalf("order/provenance %+v", reg)
			}
			requests, err := state.OpenReadOnly(home).LoadRequests()
			if err != nil || len(requests) != 1 || len(requests[0].HookEvidence) != 3 {
				t.Fatalf("replay evidence %+v %v", requests, err)
			}
			entries, err = os.ReadDir(admissionIntentDir(home))
			if err != nil || len(entries) != 0 {
				t.Fatalf("ack %+v %v", entries, err)
			}
		})
	}
}

func TestDirectAndContendedBatchPreserveNativeLocatorOrder(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	run := func(contended bool) string {
		home, project := t.TempDir(), t.TempDir()
		setUpTestConfig(t, home, project, at.Add(-time.Hour))
		follow := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalAnswer, "same-native", project, "/earlier"), ObservedAt: at})
		start := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalBegin, "same-native", project, ""), ObservedAt: at})
		later := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalAnswer, "same-native", project, "/later"), ObservedAt: at})
		later[0].Reason, later[0].NativeEvent = "later", "later"
		batch := append(append(follow, start...), later...)
		if err := validateBatchStructure("synthetic", batch, at); err != nil {
			t.Fatal(err)
		}
		release := func() {}
		if contended {
			var err error
			release, err = local.NamedLock(home, "hooks.lock")
			if err != nil {
				t.Fatal(err)
			}
		}
		err := HandleBatch(home, "synthetic", batch, at)
		release()
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= 2; i++ {
			if err := ReplayAdmissionIntents(home, at.Add(time.Duration(i)*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
		regs, err := state.OpenReadOnly(home).LoadRegistrations()
		if err != nil || len(regs) != 1 {
			t.Fatalf("registrations %v %v", regs, err)
		}
		if !regs[0].AdmittedAt.Equal(at) || regs[0].NativeSessionID != "same-native" || regs[0].Origin != archive.SessionOriginHook {
			t.Fatalf("provenance %v", regs[0])
		}
		entries, err := os.ReadDir(admissionIntentDir(home))
		if err != nil || len(entries) != 0 {
			t.Fatalf("ack %v %v", entries, err)
		}
		return regs[0].TranscriptPath
	}
	direct, contended := run(false), run(true)
	t.Logf("production HandleBatch after two successful replay passes: direct locator=%q contended locator=%q", direct, contended)
	if direct != "/earlier" || contended != "/earlier" {
		t.Errorf("FV3B-ORDER-1: same valid ordered batch has contention-dependent permanent locator")
	}
}

func TestOrderedDirectBatchSurvivesEachPartialEffect(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, boundary := range []effectName{effectRegistrationCreate, effectLocatorUpdate, effectEvidenceSave, effectRequestSave, effectIntentAck} {
		t.Run(string(boundary), func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			follow := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalAnswer, "ordered-direct", project, "/earlier"), ObservedAt: at})
			start := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalBegin, "ordered-direct", project, ""), ObservedAt: at})
			later := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalAnswer, "ordered-direct", project, "/later"), ObservedAt: at})
			later[0].Reason, later[0].NativeEvent = "later", "later"
			follow[0].Evidence = []archive.SupplementalEvidence{minimalReplayEvidence(archive.EvidenceKindFinalResponse, follow[0], at)}
			later[0].Evidence = []archive.SupplementalEvidence{minimalReplayEvidence(archive.EvidenceKindFinalResponse, later[0], at)}
			batch := append(append(follow, start...), later...)
			interrupted := false
			err := handleBatch(home, "synthetic", batch, at, nil, nil, eventOptions{afterEffect: func(name effectName) error {
				if name == boundary && !interrupted {
					interrupted = true
					return errors.New("partial ordered direct batch")
				}
				return nil
			}})
			if err == nil || !interrupted {
				t.Fatalf("missing actual failure: %v", err)
			}
			entries, err := os.ReadDir(admissionIntentDir(home))
			if err != nil {
				t.Fatal(err)
			}
			if boundary != effectIntentAck && len(entries) != 1 {
				t.Fatalf("whole batch not retained: %v", entries)
			}
			for range 2 {
				if err := ReplayAdmissionIntents(home, at.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			if err := HandleBatch(home, "synthetic", batch, at); err != nil {
				t.Fatal(err)
			}
			regs, err := state.OpenReadOnly(home).LoadRegistrations()
			if err != nil || len(regs) != 1 {
				t.Fatalf("registration %v %v", regs, err)
			}
			reg := regs[0]
			if reg.TranscriptPath != "/earlier" || !reg.AdmittedAt.Equal(at) || reg.NativeSessionID != "ordered-direct" || reg.Harness.Name != "synthetic" || reg.Origin != archive.SessionOriginHook {
				t.Fatalf("order/provenance %v", reg)
			}
			requests, err := state.OpenReadOnly(home).LoadRequests()
			if err != nil || len(requests) != 1 || len(requests[0].HookEvidence) != 3 {
				t.Fatalf("evidence %v %v", requests, err)
			}
			entries, err = os.ReadDir(admissionIntentDir(home))
			if err != nil || len(entries) != 0 {
				t.Fatalf("ack %v %v", entries, err)
			}
		})
	}
}

func TestFollowupBeforeUnsupportedStartNeverAdmits(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, fresh := range []agentapi.Freshness{agentapi.FreshUnknown, agentapi.FreshContinuation} {
		for _, contended := range []bool{false, true} {
			home, project := t.TempDir(), t.TempDir()
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			follow := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalAnswer, "unsupported", project, "/earlier"), ObservedAt: at})
			start := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalBegin, "unsupported", project, ""), ObservedAt: at})
			start[0].Start.Kind = fresh
			release := func() {}
			if contended {
				var err error
				release, err = local.NamedLock(home, "hooks.lock")
				if err != nil {
					t.Fatal(err)
				}
			}
			err := HandleBatch(home, "synthetic", append(follow, start...), at)
			release()
			if err != nil {
				t.Fatal(err)
			}
			if err := ReplayAdmissionIntents(home, at.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			regs, err := state.OpenReadOnly(home).LoadRegistrations()
			if err != nil || len(regs) != 0 {
				t.Fatalf("unsupported %v contended %t admitted %v %v", fresh, contended, regs, err)
			}
		}
	}
}

func TestOrderedBatchRequestsRecoveryAndHonorsRevokedGeneration(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, revoke := range []bool{false, true} {
		home, project := t.TempDir(), t.TempDir()
		setUpTestConfig(t, home, project, at.Add(-time.Hour))
		follow := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalAnswer, "ordered-recovery", project, "/earlier"), ObservedAt: at})
		start := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalBegin, "ordered-recovery", project, ""), ObservedAt: at})
		batch := append([]agentapi.LifecycleEvent(nil), follow...)
		batch = append(batch, start...)
		key, err := eventKey(follow[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := state.Open(home); err != nil {
			t.Fatal(err)
		}
		corruptQualifiedIndex(t, home, key)
		if err := HandleBatch(home, "synthetic", batch, at); !errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
			t.Fatalf("missing recovery: %v", err)
		}
		entries, err := os.ReadDir(admissionIntentDir(home))
		if err != nil || len(entries) != 1 {
			t.Fatalf("recovery batch not queued %v %v", entries, err)
		}
		if revoke {
			if _, err := config.SetPaused(home, true); err != nil {
				t.Fatal(err)
			}
			if _, err := config.SetPaused(home, false); err != nil {
				t.Fatal(err)
			}
		}
		store, err := state.Open(home)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecoverSessionIndex(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := ReplayAdmissionIntents(home, at.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		regs, err := store.LoadRegistrations()
		if err != nil {
			t.Fatal(err)
		}
		if revoke {
			if len(regs) != 0 {
				t.Fatal("revoked generation admitted")
			}
		} else if len(regs) != 1 || regs[0].TranscriptPath != "/earlier" || !regs[0].AdmittedAt.Equal(at) {
			t.Fatalf("recovered order/proof %v", regs)
		}
		entries, err = os.ReadDir(admissionIntentDir(home))
		if err != nil || len(entries) != 0 {
			t.Fatalf("ack %v %v", entries, err)
		}
	}
}

func TestOrderedBatchBeforeActivationKeepsDeclinedStartDiagnostic(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, at.Add(time.Hour))
	batch := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalAnswer, "preactivation", project, "/earlier"), ObservedAt: at})
	batch = append(batch, syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalBegin, "preactivation", project, ""), ObservedAt: at})...)
	if err := HandleBatch(home, "synthetic", batch, at); err != nil {
		t.Fatal(err)
	}
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("preactivation admitted %v %v", regs, err)
	}
	diagnostics, err := ReadDiagnostics(home)
	if err != nil || len(diagnostics) != 1 || diagnostics[0].Code != DiagnosticPreActivationStart {
		t.Fatalf("lost declined-start diagnostic %v %v", diagnostics, err)
	}
	entries, err := os.ReadDir(admissionIntentDir(home))
	if !os.IsNotExist(err) && (err != nil || len(entries) != 0) {
		t.Fatalf("preactivation intent %v %v", entries, err)
	}
}

const (
	replayModeA    agentapi.NativeMode = "mode-A"
	replayModeB    agentapi.NativeMode = "mode-B"
	replayModeLive agentapi.NativeMode = "mode-live"
)

// Regression OIR-ORDER-START-01: one admission batch can carry later native
// start observations after the effect that durably created its registration.
func twoReplayStarts(project string, at time.Time) []agentapi.LifecycleEvent {
	first := agentapi.LifecycleEvent{
		Kind:        agentapi.EventStart,
		Session:     agentapi.NativeSession{Agent: syntheticHookAgent, NativeID: "two-start-owner", Version: "version-A", Mode: replayModeA},
		ProjectRoot: project, Source: agentapi.SourceRef{Kind: archive.SourceKindFile, Path: "/synthetic/source-A"}, Locator: agentapi.LocatorReplaceFile,
		Start: agentapi.StartEvidence{Kind: agentapi.FreshExplicit, Reason: agentapi.FreshnessExplicitStart}, Deferred: agentapi.DeferredStart,
		Reason: "first", NativeEvent: "first",
	}
	second := first
	second.Source.Path = "/synthetic/source-B"
	second.Session.Version, second.Session.Mode = "version-B", replayModeB
	second.Reason, second.NativeEvent = "second", "second"
	for _, event := range []*agentapi.LifecycleEvent{&first, &second} {
		event.Evidence = []archive.SupplementalEvidence{minimalReplayEvidence(archive.EvidenceKindLifecycleHook, *event, at)}
	}
	return []agentapi.LifecycleEvent{first, second}
}

func TestMultipleDeferredStartsReplayPreservesNativeOrder(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, boundary := range []effectName{"", effectRegistrationCreate, effectRegistrationUpdate, effectEvidenceSave, effectIntentAck} {
		t.Run(string(boundary), func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			batch := twoReplayStarts(project, at)
			if _, err := validateBatch("synthetic", batch, at); err != nil {
				t.Fatal(err)
			}
			cfg, _, err := loadHookCaptureWindow(home, nil)
			if err != nil {
				t.Fatal(err)
			}
			queued, err := queueEventBatchInGeneration(home, batch, at, cfg.PauseGeneration, nil)
			if err != nil || !queued {
				t.Fatalf("queue %t %v", queued, err)
			}
			interrupted := false
			err = replayAdmissionIntents(home, at.Add(time.Second), nil, func(name effectName) error {
				if boundary != "" && name == boundary && !interrupted {
					interrupted = true
					return errors.New("partial multi-start replay")
				}
				return nil
			})
			if boundary == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil || !interrupted {
					t.Fatalf("durable interruption not exercised: %v", err)
				}
				entries, err := os.ReadDir(admissionIntentDir(home))
				if err != nil {
					t.Fatal(err)
				}
				if boundary != effectIntentAck && len(entries) != 1 {
					t.Fatalf("partial batch lost: %v", entries)
				}
			}
			for range 2 {
				if err := ReplayAdmissionIntents(home, at.Add(2*time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			regs, err := state.OpenReadOnly(home).LoadRegistrations()
			if err != nil || len(regs) != 1 {
				t.Fatalf("registrations %v %v", regs, err)
			}
			reg := regs[0]
			if reg.NativeSessionID != batch[0].Session.NativeID || reg.Harness.Name != "synthetic" || reg.ProjectRoot != project || reg.DestinationID != cfg.DestinationID() || !sameReplayAdmission(reg, at) {
				t.Fatalf("admission provenance changed: %+v", reg)
			}
			if reg.TranscriptPath != batch[1].Source.Path || reg.Harness.Version != batch[1].Session.Version || reg.Harness.Mode != string(batch[1].Session.Mode) {
				t.Fatalf("later native start permanently skipped: %+v", reg)
			}
			requests, err := state.OpenReadOnly(home).LoadRequests()
			if err != nil || len(requests) != 1 || len(requests[0].HookEvidence) != 2 {
				t.Fatalf("evidence %v %v", requests, err)
			}
			entries, err := os.ReadDir(admissionIntentDir(home))
			if err != nil || len(entries) != 0 {
				t.Fatalf("acknowledgement %v %v", entries, err)
			}
		})
	}
}

func TestMultiStartReplayPreservesNewOnlyAndNewerLiveObservations(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, newer := range []bool{false, true} {
		home, project := t.TempDir(), t.TempDir()
		setUpTestConfig(t, home, project, at.Add(-time.Hour))
		batch := twoReplayStarts(project, at)
		batch[1].NewOnly = !newer
		cfg, _, err := loadHookCaptureWindow(home, nil)
		if err != nil {
			t.Fatal(err)
		}
		queued, err := queueEventBatchInGeneration(home, batch, at, cfg.PauseGeneration, nil)
		if err != nil || !queued {
			t.Fatalf("queue %t %v", queued, err)
		}
		err = replayAdmissionIntents(home, at.Add(time.Second), nil, func(name effectName) error {
			if name == effectRegistrationCreate {
				return errors.New("first registration durable")
			}
			return nil
		})
		if err == nil {
			t.Fatal("missing interruption")
		}
		expected := batch[0]
		expectedRegisteredAt := at
		if newer {
			live := batch[1]
			live.Source.Path = "/synthetic/source-live"
			live.Session.Version, live.Session.Mode = "version-live", replayModeLive
			live.Start = agentapi.StartEvidence{Kind: agentapi.FreshContinuation, Reason: agentapi.FreshnessContinuation}
			live.Deferred = agentapi.DeferredNone
			live.Evidence = nil
			expectedRegisteredAt = at.Add(time.Minute)
			if err := HandleBatch(home, "synthetic", []agentapi.LifecycleEvent{live}, expectedRegisteredAt); err != nil {
				t.Fatal(err)
			}
			expected = live
		}
		for range 2 {
			if err := ReplayAdmissionIntents(home, at.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		regs, err := state.OpenReadOnly(home).LoadRegistrations()
		if err != nil || len(regs) != 1 {
			t.Fatalf("registrations %v %v", regs, err)
		}
		reg := regs[0]
		if reg.TranscriptPath != expected.Source.Path || reg.Harness.Version != expected.Session.Version || reg.Harness.Mode != string(expected.Session.Mode) || !reg.RegisteredAt.Equal(expectedRegisteredAt) || !reg.AdmittedAt.Equal(at) || !reg.SessionStartedAt.Equal(at) || reg.DestinationID != cfg.DestinationID() || reg.Origin != archive.SessionOriginHook {
			t.Fatalf("newer=%t replay replaced protected facts: %+v", newer, reg)
		}
		entries, err := os.ReadDir(admissionIntentDir(home))
		if err != nil || len(entries) != 0 {
			t.Fatalf("acknowledgement %v %v", entries, err)
		}
	}
}
