package backfill

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"os"
	"path/filepath"
	"testing"
)

func TestDurableImportPublishesParentAndChildrenAfterNativeDeletion(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	at := fixedNow.UTC()
	start := at.Add(-100000000000)
	cfg := config.Config{DurableImportProtection: true, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{ProjectID: archive.ProjectID(project), Root: project, Included: true, ActivatedAt: start}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	parentPath, childPath := filepath.Join(project, "parent.jsonl"), filepath.Join(project, "agent-child.jsonl")
	if err = os.WriteFile(parentPath, []byte(claudeTranscript("parent", project, start)), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(childPath, []byte(subagentTranscript("parent", "child", start.Add(1000000000))), 0600); err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{Harness: "claude-code", NativeSessionID: "parent", TranscriptPath: parentPath, ProjectRoot: project, StartedAt: start, StartedAtSource: archive.StartedAtSourceTranscript, Subagents: []Subagent{{Path: childPath, AgentID: "child"}}}
	reg := Registration{Durable: true, Sources: testSources, Home: home, Store: store, Batch: "synthetic", AdmittedAt: at}
	result, err := reg.Run([]Candidate{candidate})
	if err != nil || len(result.Sessions) != 1 || len(result.Subagents) != 1 {
		t.Fatal(result, err)
	}
	for _, id := range append(result.Sessions, result.Subagents...) {
		r, found, e := store.LoadRegistration(id)
		if e != nil || !found || r.AdmissionStage == "" {
			t.Fatal(r, found, e)
		}
	}
	if err = os.RemoveAll(project); err != nil {
		t.Fatal(err)
	}
	// A restart uses a fresh store and never asks Git or a native reader.
	store = state.OpenReadOnly(home)
	cloud := storagetest.NewMemoryStore()
	opts := collector.Options{Sources: testSources, Parsers: testSources, MachineID: "synthetic-machine", RepoKey: func(string) string { t.Fatal("staged ownership reran Git"); return "" }}
	published, err := collector.Run(t.Context(), store, cloud, opts)
	if err != nil || len(published.Errors) != 0 || len(published.Published) != 2 {
		t.Fatal(published, err)
	}
	for _, id := range append(result.Sessions, result.Subagents...) {
		r, _, _ := store.LoadRegistration(id)
		released, e := store.AdmissionStageReleased(r)
		if e != nil || !released {
			t.Fatal("stage not released", id, e)
		}
	}
}

func TestDurableImportCannotAdmitWithoutCommittedFence(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	cfg := config.Config{Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{ProjectID: archive.ProjectID(project), Root: project, Included: true}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "parent.jsonl")
	if err := os.WriteFile(path, []byte(claudeTranscript("parent", project, fixedNow.Add(-100000000000))), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (Registration{Durable: true, Sources: testSources, Home: home, Store: store, Batch: "synthetic", AdmittedAt: fixedNow}).Run([]Candidate{{Harness: "claude-code", NativeSessionID: "parent", TranscriptPath: path, ProjectRoot: project, StartedAt: fixedNow.Add(-100000000000)}})
	if err == nil || len(result.Sessions) != 0 {
		t.Fatal(result, err)
	}
	regs, e := store.LoadRegistrations()
	if e != nil || len(regs) != 0 {
		t.Fatal(regs, e)
	}
	if errors.Is(err, state.ErrAdmissionStageCapacity) {
		t.Fatal("wrong refusal", err)
	}
}

func TestDurableImportHeaderlessExceptionRequiresDeclaredPolicyAndEmptyReview(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native.txt")
	if err := os.WriteFile(path, []byte("user: synthetic conversation\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, harness := range []string{"cursor", "claude-code"} {
		t.Run(harness, func(t *testing.T) {
			candidate := Candidate{Harness: harness, NativeSessionID: "native", TranscriptPath: path, StartedAt: fixedNow.Add(-100000000000), reviewedHeader: &agentapi.NativeHeader{Directory: "/changed"}}
			if harness == "claude-code" {
				candidate.reviewedHeader = &agentapi.NativeHeader{}
			}
			lookup := missingHeaderSources{SourcesLookup: testSources, ImportsLookup: testSources}
			reg := archive.SessionRegistration{ArchiveSessionID: "synthetic", NativeSessionID: "native", Harness: archive.Harness{Name: harness}, Origin: archive.SessionOriginImport}
			if _, err := (Registration{Sources: lookup, AdmittedAt: fixedNow}).materialize(t.Context(), candidate, &reg, nil, nil); err == nil {
				t.Fatal("undeclared or changed reviewed header admitted")
			}
		})
	}
}

type missingHeaderSources struct {
	agentapi.SourcesLookup
	agentapi.ImportsLookup
}

func (missingHeaderSources) LookupNativeHeaders(string) (agentapi.NativeHeaderInspector, bool) {
	return nil, false
}
func (missingHeaderSources) NativeHeaderAgents() []string { return nil }
