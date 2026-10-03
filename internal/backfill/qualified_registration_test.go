package backfill

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestQualifiedBackfillKeepsParentChildIDsAcrossInterruptedHolds(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(map[bool]string{false: "restart after child candidate", true: "reservation removed before parent commit"}[expire], func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
			cfg := config.Config{Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{ProjectID: archive.ProjectID(project), Root: project, Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
			if err := config.Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			store, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			parentPath, childPath := filepath.Join(project, "parent.jsonl"), filepath.Join(project, "child.jsonl")
			for _, path := range []string{parentPath, childPath} {
				if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "parent"}
			c := Candidate{Harness: "claude-code", NativeSessionID: key.NativeID, TranscriptPath: parentPath, ProjectRoot: project, StartedAt: at.Add(-time.Minute), StartedAtSource: archive.StartedAtSourceTranscript, Subagents: []Subagent{{AgentID: "child", Path: childPath}}}
			var original state.SubagentCandidate
			observed := false
			r := Registration{Home: home, Store: store, Batch: "2026-09-22-1", AdmittedAt: at, MaxHoldSteps: 1, Stop: func() bool {
				if observed {
					return false
				}
				candidates, err := store.LoadSubagentCandidates()
				if err != nil {
					t.Fatal(err)
				}
				if len(candidates) == 0 {
					return false
				}
				if len(candidates) != 1 {
					t.Fatalf("candidates %#v", candidates)
				}
				original, observed = candidates[0], true
				if expire {
					if err := store.ForgetSession(original.ParentArchiveSessionID, key); err != nil {
						t.Fatal(err)
					}
					return false
				}
				return true
			}}
			result, err := r.Run([]Candidate{c})
			if expire {
				if err == nil || len(result.Sessions) != 0 {
					t.Fatalf("stale parent committed %#v %v", result, err)
				}
				regs, err := store.LoadRegistrations()
				if err != nil || len(regs) != 0 {
					t.Fatalf("expired parent revived %#v %v", regs, err)
				}
				return
			}
			if !errors.Is(err, ErrStopped) {
				t.Fatalf("missing interruption %v", err)
			}
			r.Store = state.OpenReadOnly(home)
			r.Stop = nil
			result, err = r.Run([]Candidate{c})
			if err != nil || len(result.Sessions) != 1 || result.Sessions[0] != original.ParentArchiveSessionID {
				t.Fatalf("parent reassigned %#v %v", result, err)
			}
			candidates, err := store.LoadSubagentCandidates()
			if err != nil || len(candidates) != 1 || candidates[0].ArchiveSessionID != original.ArchiveSessionID || candidates[0].ParentArchiveSessionID != original.ParentArchiveSessionID {
				t.Fatalf("child reassigned %#v %v", candidates, err)
			}
			parent, found, err := store.LoadRegistration(result.Sessions[0])
			if err != nil || !found || parent.Origin != archive.SessionOriginImport || !parent.InBatch("2026-09-22-1") || !parent.AdmittedAt.Equal(at) {
				t.Fatalf("provenance changed %#v %v", parent, err)
			}
		})
	}
}
