package capture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

type injectedDecoder struct {
	decode func(agentapi.HookInput) []agentapi.LifecycleEvent
}

func (d injectedDecoder) Decode(_ context.Context, in agentapi.HookInput) ([]agentapi.LifecycleEvent, error) {
	return d.decode(in), nil
}

type injectedLookup struct{ decoder agentapi.HookDecoder }

func (d injectedLookup) LookupDecoder(name string) (agentapi.HookDecoder, bool) {
	return d.decoder, name == "synthetic"
}
func syntheticLifecycle(in agentapi.HookInput) []agentapi.LifecycleEvent {
	identity, _ := in.Payload["opaque"].(string)
	project, _ := in.Payload["checkout"].(string)
	path, _ := in.Payload["locator"].(string)
	signal, _ := in.Payload["signal"].(string)
	event := agentapi.LifecycleEvent{Session: agentapi.NativeSession{Agent: "synthetic", NativeID: identity}, ProjectRoot: project, Reason: signal, NativeEvent: signal, Source: agentapi.SourceRef{Path: path}}
	switch signal {
	case "begin":
		event.Kind = agentapi.EventStart
		event.NewOnly = true
		event.Start = agentapi.StartEvidence{Kind: agentapi.FreshExplicit, Reason: "explicit_start"}
		event.Deferred = agentapi.DeferredStart
		turn := event
		turn.Kind = agentapi.EventTurnStart
		turn.NewOnly = false
		turn.Start = agentapi.StartEvidence{}
		turn.Deferred = agentapi.DeferredNone
		turn.Evidence = []archive.SupplementalEvidence{{Kind: archive.EvidenceKindLifecycleHook, ObservedAt: in.ObservedAt, Provenance: "hook:synthetic:begin", Payload: map[string]any{"event_name": "begin"}}}
		return []agentapi.LifecycleEvent{event, turn}
	case "answer":
		event.Kind = agentapi.EventResponse
		event.Locator = agentapi.LocatorFillFile
		event.Deferred = agentapi.DeferredFollowup
	case "finish":
		event.Kind = agentapi.EventStop
	case "child":
		event.Kind = agentapi.EventSubagent
		event.Child = &agentapi.ChildObservation{ID: "child-1", Path: path, Type: "synthetic", CaptureTranscript: true}
	default:
		return nil
	}
	return []agentapi.LifecycleEvent{event}
}
func syntheticPayload(signal, id, project, path string) map[string]any {
	return map[string]any{"signal": signal, "opaque": id, "checkout": project, "locator": path}
}
func TestInjectedDecoderUsesRealAdmissionAndChildEffects(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	ports := injectedLookup{injectedDecoder{syntheticLifecycle}}
	id := " opaque/λ\x00 "
	// An absent-parent follow-up can retain a locator but cannot admit by itself.
	if err := HandleEvent(home, "synthetic", syntheticPayload("answer", id, project, "/synthetic/source"), at, WithDecoders(ports)); err != nil {
		t.Fatal(err)
	}
	if regs, err := state.OpenReadOnly(home).LoadRegistrations(); err != nil || len(regs) != 0 {
		t.Fatalf("follow-up admitted: %v %v", regs, err)
	}
	if err := HandleEvent(home, "synthetic", syntheticPayload("begin", id, project, ""), at, WithDecoders(ports)); err != nil {
		t.Fatal(err)
	}
	if err := ReplayAdmissionIntents(home, at.Add(time.Second), ports); err != nil {
		t.Fatal(err)
	}
	key, _ := agentmeta.NewSessionKey("synthetic", id)
	store := state.OpenReadOnly(home)
	archiveID, found, err := store.ArchiveSessionID(key)
	if err != nil || !found {
		t.Fatalf("lookup %q %v %v", archiveID, found, err)
	}
	reg, found, err := store.LoadRegistration(archiveID)
	if err != nil || !found || reg.NativeSessionID != id || reg.TranscriptPath != "/synthetic/source" || reg.ProjectRoot != project || reg.DestinationID == "" {
		t.Fatalf("registration %+v %v", reg, err)
	}
	// A built-in conversation carrying identical opaque bytes has another owner.
	if err := HandleEvent(home, "claude", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": id, "cwd": project}, at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	builtinID, found, err := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: id})
	if err != nil || !found || builtinID == archiveID {
		t.Fatalf("colliding native owners %q %q %v", builtinID, archiveID, err)
	}
	if err := HandleEvent(home, "synthetic", syntheticPayload("child", id, project, "/synthetic/child"), at, WithDecoders(ports)); err != nil {
		t.Fatal(err)
	}
	children, err := store.LoadSubagentCandidates()
	if err != nil || len(children) != 1 || children[0].ParentArchiveSessionID != archiveID || children[0].ParentNativeSessionID != id || children[0].ProjectRoot != reg.ProjectRoot {
		t.Fatalf("children %+v %v", children, err)
	}
	if err := HandleEvent(home, "synthetic", syntheticPayload("finish", id, project, ""), at, WithDecoders(ports)); err != nil {
		t.Fatal(err)
	}
	requests, err := store.LoadRequests()
	urgent := false
	for _, request := range requests {
		if request.ArchiveSessionID == archiveID && request.Urgent() {
			urgent = true
		}
	}
	if err != nil || !urgent {
		t.Fatalf("requests %+v %v", requests, err)
	}
}
func TestBatchRejectsEveryInvalidLaterEffectBeforeAnyMutation(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for name, corrupt := range map[string]func(*agentapi.LifecycleEvent){
		"identity": func(e *agentapi.LifecycleEvent) { e.Session.NativeID = "other" },
		"agent":    func(e *agentapi.LifecycleEvent) { e.Session.Agent = "claude" },
		"project":  func(e *agentapi.LifecycleEvent) { e.ProjectRoot = "/other" },
		"kind":     func(e *agentapi.LifecycleEvent) { e.Kind = 255 },
		"source":   func(e *agentapi.LifecycleEvent) { e.Source.Key = "database" },
		"evidence": func(e *agentapi.LifecycleEvent) { e.Evidence[0].ObservedAt = at.Add(time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			before, err := os.ReadDir(home)
			if err != nil {
				t.Fatal(err)
			}
			ports := injectedLookup{injectedDecoder{func(in agentapi.HookInput) []agentapi.LifecycleEvent {
				batch := syntheticLifecycle(in)
				corrupt(&batch[1])
				return batch
			}}}
			probes := 0
			err = HandleEvent(home, "synthetic", syntheticPayload("begin", "native", project, ""), at, WithDecoders(ports), WithRepoKey(func(string) string { probes++; return "" }))
			if err == nil || probes != 0 {
				t.Fatalf("err %v probes %d", err, probes)
			}
			after, err := os.ReadDir(home)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("invalid batch mutated disk: %v %v", after, err)
			}
		})
	}
}
func TestReplayAfterEachDurableLifecycleBoundaryKeepsIdentity(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, boundary := range []string{"registration-create", "evidence-save", "locator-update", "request-save", "child-reservation", "child-link", "child-candidate"} {
		t.Run(boundary, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			ports := injectedLookup{injectedDecoder{syntheticLifecycle}}
			start := syntheticPayload("begin", "native", project, "")
			payload := start
			if boundary != "registration-create" && boundary != "evidence-save" {
				if err := HandleEvent(home, "synthetic", start, at, WithDecoders(ports)); err != nil {
					t.Fatal(err)
				}
				switch boundary {
				case "locator-update", "request-save":
					payload = syntheticPayload("answer", "native", project, "/source")
				default:
					payload = syntheticPayload("child", "native", project, "/child")
				}
			}
			interrupted := false
			fail := func(o *eventOptions) {
				o.afterEffect = func(name string) error {
					if name == boundary && !interrupted {
						interrupted = true
						return errors.New("durable interruption")
					}
					return nil
				}
			}
			if err := HandleEvent(home, "synthetic", payload, at, WithDecoders(ports), fail); err == nil || !interrupted {
				t.Fatalf("fault boundary not exercised: %v %v", err, interrupted)
			}
			key, _ := agentmeta.NewSessionKey("synthetic", "native")
			id, found, err := state.OpenReadOnly(home).ArchiveSessionID(key)
			if err != nil || !found {
				t.Fatalf("durable identity %s %v %v", id, found, err)
			}
			if err := HandleEvent(home, "synthetic", payload, at, WithDecoders(ports)); err != nil {
				t.Fatal(err)
			}
			if err := HandleEvent(home, "synthetic", payload, at, WithDecoders(ports)); err != nil {
				t.Fatal(err)
			}
			store := state.OpenReadOnly(home)
			regs, err := store.LoadRegistrations()
			if err != nil || len(regs) != 1 || regs[0].ArchiveSessionID != id || !regs[0].AdmittedAt.Equal(at) {
				t.Fatalf("replay identity/provenance %+v %v", regs, err)
			}
			requests, err := store.LoadRequests()
			if err != nil || len(requests) != 1 {
				t.Fatalf("replay request %+v %v", requests, err)
			}
			seen := map[string]bool{}
			for _, e := range requests[0].HookEvidence {
				key := string(e.Kind) + e.Provenance + e.ObservedAt.String()
				if seen[key] {
					t.Fatalf("duplicate replay evidence %+v", requests[0])
				}
				seen[key] = true
			}
			if strings.HasPrefix(boundary, "child-") {
				children, err := store.LoadSubagentCandidates()
				if err != nil || len(children) != 1 {
					t.Fatalf("child replay %+v %v", children, err)
				}
			}
		})
	}
}
func TestLegacyAdmissionBytesTranslateWithoutRestatting(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	path := filepath.Join(t.TempDir(), "native.jsonl")
	if err := os.WriteFile(path, []byte("grown transcript"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadHookCaptureWindow(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy := agentapi.LegacyAdmission{Harness: "claude", Event: "SessionStart", NativeSessionID: "native", ProjectRoot: project, DestinationID: cfg.DestinationID(), TranscriptPath: path, ObservedAt: at, PauseGeneration: cfg.PauseGeneration}
	if err := local.Write(filepath.Join(admissionIntentDir(home), "legacy.json"), legacy); err != nil {
		t.Fatal(err)
	}
	if err := ReplayAdmissionIntents(home, at.Add(time.Second), testDecoders); err != nil {
		t.Fatal(err)
	}
	key, _ := agentmeta.NewSessionKey("claude", "native")
	id, found, err := state.OpenReadOnly(home).ArchiveSessionID(key)
	if err != nil || !found {
		t.Fatalf("legacy proof lost %s %v %v", id, found, err)
	}
	if _, err := os.Stat(filepath.Join(admissionIntentDir(home), "legacy.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy intent not acknowledged: %v", err)
	}
}
func TestQueuedReplayInterruptionCompletesEveryDurableEffect(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, boundary := range []string{"registration-create", "evidence-save", "locator-update", "request-save", "intent-ack"} {
		t.Run(boundary, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			ports := injectedLookup{injectedDecoder{syntheticLifecycle}}
			payload := syntheticPayload("begin", "native", project, "")
			if boundary == "locator-update" || boundary == "request-save" {
				if err := HandleEvent(home, "synthetic", payload, at, WithDecoders(ports)); err != nil {
					t.Fatal(err)
				}
				payload = syntheticPayload("answer", "native", project, "/source")
			}
			batch := syntheticLifecycle(agentapi.HookInput{Payload: payload, ObservedAt: at})
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
			err = replayAdmissionIntents(home, at.Add(time.Second), ports, func(name string) error {
				if name == boundary && !interrupted {
					interrupted = true
					return errors.New("after durable replay effect")
				}
				return nil
			})
			if err == nil || !interrupted {
				t.Fatalf("replay failure boundary absent %v %v", err, interrupted)
			}
			key, _ := agentmeta.NewSessionKey("synthetic", "native")
			id, found, err := state.OpenReadOnly(home).ArchiveSessionID(key)
			if err != nil || !found {
				t.Fatalf("registered replay %s %v %v", id, found, err)
			}
			entries, err := os.ReadDir(admissionIntentDir(home))
			if err != nil {
				t.Fatal(err)
			}
			if boundary != "intent-ack" && len(entries) != 1 {
				t.Fatalf("partial replay lost intent %v", entries)
			}
			if err := ReplayAdmissionIntents(home, at.Add(2*time.Second), ports); err != nil {
				t.Fatal(err)
			}
			if err := ReplayAdmissionIntents(home, at.Add(3*time.Second), ports); err != nil {
				t.Fatal(err)
			}
			store := state.OpenReadOnly(home)
			reg, found, err := store.LoadRegistration(id)
			if err != nil || !found || !reg.AdmittedAt.Equal(at) || reg.Origin != archive.SessionOriginHook {
				t.Fatalf("replay changed provenance %+v %v", reg, err)
			}
			requests, err := store.LoadRequests()
			if err != nil || len(requests) != 1 || len(requests[0].HookEvidence) == 0 {
				t.Fatalf("replay lost evidence %+v %v", requests, err)
			}
			if boundary == "locator-update" || boundary == "request-save" {
				if reg.TranscriptPath != "/source" || !requests[0].Urgent() {
					t.Fatalf("followup effects incomplete %+v %+v", reg, requests)
				}
			} else if len(requests[0].HookEvidence) != 1 {
				t.Fatalf("duplicate start evidence %+v", requests)
			}
			entries, err = os.ReadDir(admissionIntentDir(home))
			if err != nil || len(entries) != 0 {
				t.Fatalf("complete replay left intent %v %v", entries, err)
			}
		})
	}
}
func TestInvalidReplayBatchPrecedesAnyAdmissionEffect(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	cfg, _, err := loadHookCaptureWindow(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	batch := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload("begin", "native", project, ""), ObservedAt: at})
	batch[1].Session.NativeID = "wrong-owner"
	batch[1].Evidence = nil
	intent := admissionIntent{Version: 1, Harness: "synthetic", NativeSessionID: "native", ProjectRoot: project, DestinationID: cfg.DestinationID(), PauseGeneration: cfg.PauseGeneration, ObservedAt: at, Effects: []agentapi.ReplayEffect{{Event: batch[0]}, {Event: batch[1]}}}
	path := filepath.Join(admissionIntentDir(home), "invalid.json")
	if err := local.Write(path, intent); err != nil {
		t.Fatal(err)
	}
	if err := ReplayAdmissionIntents(home, at.Add(time.Second)); err == nil {
		t.Fatal("invalid replay accepted")
	}
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("late invalid effect admitted earlier effect %v %v", regs, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("invalid replay was silently discarded: %v", err)
	}
}
