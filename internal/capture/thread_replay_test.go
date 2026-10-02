package capture

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
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
