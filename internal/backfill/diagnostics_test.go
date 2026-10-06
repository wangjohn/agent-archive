package backfill

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

func TestRecoveryOutcomesSurvivePlanningAndPrecedence(t *testing.T) {
	outcomes := []sourcefacts.RecoveryOutcome{sourcefacts.RecoveryBudgetExhausted, sourcefacts.RecoveryInventoryUnavailable, sourcefacts.RecoveryExcluded, sourcefacts.RecoveryAmbiguous, sourcefacts.RecoveryRepositoryUnavailable, sourcefacts.RecoveryMappingConflict, sourcefacts.RecoverySubtreeUnavailable, sourcefacts.RecoveryUnavailable}
	for _, outcome := range outcomes {
		t.Run(string(outcome), func(t *testing.T) {
			w := &work{t: &transcript{}, c: Candidate{StartedAt: fixedNow.Add(-time.Hour)}, res: resolution{skip: SkipWorktreeUnresolved, outcome: outcome}}
			decidePlanCandidates([]*work{w}, time.Time{}, time.Time{}, fixedNow)
			if w.c.Skip != SkipWorktreeUnresolved || w.c.Diagnostic == nil || w.c.Diagnostic.Detail != DiagnosticDetail(outcome) {
				t.Fatalf("lost outcome: %+v", w.c)
			}
			w.state = SkipAlreadyArchived
			decidePlanCandidates([]*work{w}, time.Time{}, time.Time{}, fixedNow)
			if w.c.Skip != SkipAlreadyArchived || w.c.Diagnostic != nil {
				t.Fatalf("diagnostic overrode precedence: %+v", w.c)
			}
		})
	}
}

func TestDeletedWorktreeResolverRetainsUnavailableAndAmbiguousEvidence(t *testing.T) {
	tr := newTree(t)
	a, b := tr.repo("home/a"), tr.repo("home/b")
	gone := tr.path("home/.codex/worktrees/gone/repo")
	key := archive.RepoKey("https://example.test/acme/repo")
	for _, known := range []bool{false, true} {
		env := tr.env()
		env.RepositoryIdentity = func(_ context.Context, root string) sourcefacts.RepositoryIdentity {
			return sourcefacts.RepositoryIdentity{Root: root, Key: key, Known: known}
		}
		cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(a, true), project(b, true)}}}
		r := newResolver(env, cfg, Filters{})
		want := sourcefacts.RecoveryInventoryUnavailable
		if known {
			want = sourcefacts.RecoveryAmbiguous
		}
		for range 2 {
			got := r.resolveEvidence(t.Context(), gone, key)
			if got.skip != SkipWorktreeUnresolved || got.outcome != want {
				t.Fatalf("got %+v, want %s", got, want)
			}
		}
	}
}

func TestDiagnosticsAreBoundedContentFreeAndJSONOptional(t *testing.T) {
	p := Plan{Filters: Filters{Harnesses: []string{"codex"}}}
	for range 5000 {
		p.Candidates = append(p.Candidates, Candidate{Harness: "codex", NativeSessionID: "private-native-id", TranscriptPath: "/private/native/locator.jsonl", SourceKey: "credential-secret", Skip: SkipWorktreeUnresolved, Diagnostic: candidateDiagnostic(SkipWorktreeUnresolved, sourcefacts.RecoveryAmbiguous)})
	}
	p.Candidates = append(p.Candidates, Candidate{Skip: SkipWorktreeUnresolved, Diagnostic: &Diagnostic{Detail: "untrusted transcript content"}})
	var text, structured bytes.Buffer
	RenderText(&text, p)
	if err := RenderJSON(&structured, p); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{text.String(), structured.String()} {
		for _, secret := range []string{"private-native-id", "locator.jsonl", "credential-secret", "untrusted transcript content"} {
			if strings.Contains(output, secret) {
				t.Fatalf("leaked %s", secret)
			}
		}
		if len(output) > 4000 {
			t.Fatalf("unbounded summary: %d", len(output))
		}
	}
	if !strings.Contains(text.String(), "multiple configured repositories") {
		t.Fatal(text.String())
	}
	var got struct {
		Diagnostics []DiagnosticSummary  `json:"diagnostics"`
		Inventory   *InventoryAccounting `json:"inventory"`
	}
	if err := json.Unmarshal(structured.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Diagnostics) != 1 || got.Diagnostics[0].Count != 5000 || got.Inventory == nil || !got.Inventory.LogicalHistoryPending {
		t.Fatalf("%+v", got)
	}
	structured.Reset()
	if err := RenderJSON(&structured, Plan{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(structured.String(), `"diagnostics"`) || strings.Contains(structured.String(), `"inventory"`) {
		t.Fatal("ordinary compatibility fields changed")
	}
}

func TestInventoryCountsPhysicalCandidatesWithoutOverlappingRoles(t *testing.T) {
	p := Plan{Candidates: []Candidate{
		{TranscriptPath: "/synthetic/a"}, {TranscriptPath: "/synthetic/a"},
		{TranscriptPath: "/synthetic/b", Skip: SkipDuplicateSession},
		{TranscriptPath: "/synthetic/c", Skip: SkipRelatedHistory},
		{TranscriptPath: "/synthetic/d", Skip: SkipExcludedProject},
		{TranscriptPath: "/synthetic/e", Skip: SkipAlreadyArchived}, {},
	}}
	a := p.InventoryAccounting()
	total := 0
	for _, n := range a.Dispositions {
		total += n
	}
	if total != 5 || a.UniqueCandidateFiles != 5 || a.DatabaseCandidates != 1 || a.Dispositions["duplicate_candidate"] != 1 || a.Dispositions["unresolved"] != 1 || !a.LogicalHistoryPending {
		t.Fatalf("%+v", a)
	}
	if d := candidateDiagnostic(SkipRelatedHistory, ""); d.Detail != "history_lookup_pending" || d.Action != ActionAwaitSupport {
		t.Fatal(d)
	}
}
