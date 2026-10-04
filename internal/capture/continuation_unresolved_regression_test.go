package capture

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestContinuationRefusesUnresolvedExcludedCheckoutBeforeMutation(t *testing.T) {
	for _, source := range []string{"resume", "compact"} {
		for _, suffix := range []string{"", "descendant"} {
			t.Run(source+"/"+suffix, func(t *testing.T) {
				home, parent, target := t.TempDir(), t.TempDir(), t.TempDir()
				child := filepath.Join(parent, "excluded")
				if err := os.Symlink(target, child); err != nil {
					t.Fatal(err)
				}
				at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
				setUpTestConfig(t, home, parent, at.Add(-time.Hour))
				original := filepath.Join(t.TempDir(), "original.jsonl")
				if err := HandleEvent(home, "claude", claudeStart(parent, "native-c1", "startup", original), at, WithDecoders(testDecoders)); err != nil {
					t.Fatal(err)
				}
				cfg, found, err := config.Load(home)
				if err != nil || !found {
					t.Fatalf("config found=%t err=%v", found, err)
				}
				cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: child, ProjectID: archive.ProjectID(child), Included: false, ActivatedAt: at.Add(-time.Hour)})
				if err := config.Save(home, cfg); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				store := state.OpenReadOnly(home)
				before, err := store.LoadRegistrations()
				if err != nil || len(before) != 1 {
					t.Fatalf("initial registrations=%+v err=%v", before, err)
				}
				evidenceBefore, err := store.StoredEvidence(before[0].ArchiveSessionID)
				if err != nil {
					t.Fatal(err)
				}
				replacement := filepath.Join(t.TempDir(), "replacement.jsonl")
				err = HandleEvent(home, "claude", claudeStart(filepath.Join(child, suffix), "native-c1", source, replacement), at.Add(time.Minute), WithDecoders(testDecoders))
				if err != nil && !errors.Is(err, errSessionIdentityConflict) {
					t.Fatalf("unexpected rejection: %v", err)
				}
				after, err := store.LoadRegistrations()
				if err != nil {
					t.Fatal(err)
				}
				evidenceAfter, err := store.StoredEvidence(before[0].ArchiveSessionID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Errorf("unresolved excluded continuation mutated registration: before=%+v after=%+v", before, after)
				}
				if !reflect.DeepEqual(evidenceBefore, evidenceAfter) {
					t.Errorf("unresolved excluded continuation mutated evidence: before=%+v after=%+v", evidenceBefore, evidenceAfter)
				}
			})
		}
	}
}

func TestContinuationKeepsResolvedIncludedWorktreeControl(t *testing.T) {
	home, parent := t.TempDir(), t.TempDir()
	cwd := filepath.Join(parent, "worktree")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, parent, at.Add(-time.Hour))
	if err := HandleEvent(home, "claude", claudeStart(cwd, "native-control", "startup", ""), at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	for i, source := range []string{"resume", "compact"} {
		replacement := filepath.Join(t.TempDir(), "replacement.jsonl")
		if err := HandleEvent(home, "claude", claudeStart(cwd, "native-control", source, replacement), at.Add(time.Duration(i+1)*time.Minute), WithDecoders(testDecoders)); err != nil {
			t.Fatal(err)
		}
		regs, err := state.OpenReadOnly(home).LoadRegistrations()
		if err != nil || len(regs) != 1 || regs[0].ProjectRoot != parent || regs[0].TranscriptPath != replacement || !regs[0].SessionStartedAt.Equal(at) {
			t.Fatalf("included continuation control=%+v err=%v", regs, err)
		}
	}
}

func TestContinuationPreservesResolvedNoOwnerAndDeclinesUnknownRule(t *testing.T) {
	for _, unresolvedRule := range []bool{false, true} {
		t.Run(map[bool]string{false: "resolved no owner", true: "unresolved rule"}[unresolvedRule], func(t *testing.T) {
			home, parent, cwd := t.TempDir(), t.TempDir(), t.TempDir()
			at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
			setUpTestConfig(t, home, parent, at.Add(-time.Hour))
			original := filepath.Join(t.TempDir(), "original.jsonl")
			if err := HandleEvent(home, "claude", claudeStart(parent, "native-status", "startup", original), at, WithDecoders(testDecoders)); err != nil {
				t.Fatal(err)
			}
			if unresolvedRule {
				broken := filepath.Join(parent, "broken")
				if err := os.Symlink(filepath.Join(t.TempDir(), "absent"), broken); err != nil {
					t.Fatal(err)
				}
				cfg, _, err := config.Load(home)
				if err != nil {
					t.Fatal(err)
				}
				cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: broken, Included: false})
				if err := config.Save(home, cfg); err != nil {
					t.Fatal(err)
				}
				cwd = parent
			}
			store := state.OpenReadOnly(home)
			before, err := store.LoadRegistrations()
			if err != nil || len(before) != 1 {
				t.Fatalf("registrations=%+v err=%v", before, err)
			}
			evidenceBefore, err := store.StoredEvidence(before[0].ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			replacement := filepath.Join(t.TempDir(), "replacement.jsonl")
			if err := HandleEvent(home, "claude", claudeStart(cwd, "native-status", "resume", replacement), at.Add(time.Minute), WithDecoders(testDecoders)); err != nil {
				t.Fatal(err)
			}
			after, err := store.LoadRegistrations()
			if err != nil || len(after) != 1 {
				t.Fatalf("registrations=%+v err=%v", after, err)
			}
			evidenceAfter, err := store.StoredEvidence(before[0].ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			if unresolvedRule {
				if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(evidenceBefore, evidenceAfter) {
					t.Fatalf("unknown rule mutated admitted registration/evidence: before=%+v after=%+v", before, after)
				}
			} else if after[0].TranscriptPath != replacement || after[0].ProjectRoot != parent || !after[0].SessionStartedAt.Equal(at) {
				t.Fatalf("resolved unmatched cwd changed admission semantics: %+v", after)
			}
		})
	}
}

type continuationScopeShape string

const (
	continuationAbsentCase       continuationScopeShape = "absent case"
	continuationAbsentUnicode    continuationScopeShape = "absent Unicode"
	continuationConflictingRules continuationScopeShape = "conflicting rules"
)

func TestContinuationDeclinesAmbiguousScopeBeforeMutation(t *testing.T) {
	for _, shape := range []continuationScopeShape{continuationAbsentCase, continuationAbsentUnicode, continuationConflictingRules} {
		t.Run(string(shape), func(t *testing.T) {
			home, parent := t.TempDir(), t.TempDir()
			at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
			setUpTestConfig(t, home, parent, at.Add(-time.Hour))
			original := filepath.Join(t.TempDir(), "original.jsonl")
			if err := HandleEvent(home, "claude", claudeStart(parent, "native-ambiguous", "startup", original), at, WithDecoders(testDecoders)); err != nil {
				t.Fatal(err)
			}
			cfg, _, err := config.Load(home)
			if err != nil {
				t.Fatal(err)
			}
			cwd, rule := parent, parent
			switch shape {
			case continuationAbsentCase:
				cwd, rule = filepath.Join(parent, "absent"), filepath.Join(parent, "ABSENT")
			case continuationAbsentUnicode:
				cwd, rule = filepath.Join(parent, "café"), filepath.Join(parent, "café")
			case continuationConflictingRules:
				// Keep both locations at parent to exercise conflicting rules.
			}
			cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: rule, Included: false})
			if err := config.Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			store := state.OpenReadOnly(home)
			before, err := store.LoadRegistrations()
			if err != nil || len(before) != 1 {
				t.Fatalf("registrations=%+v err=%v", before, err)
			}
			evidenceBefore, err := store.StoredEvidence(before[0].ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			replacement := filepath.Join(t.TempDir(), "replacement.jsonl")
			if err := HandleEvent(home, "claude", claudeStart(cwd, "native-ambiguous", "resume", replacement), at.Add(time.Minute), WithDecoders(testDecoders)); err != nil {
				t.Fatal(err)
			}
			after, err := store.LoadRegistrations()
			if err != nil {
				t.Fatal(err)
			}
			evidenceAfter, err := store.StoredEvidence(before[0].ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(evidenceBefore, evidenceAfter) {
				t.Fatalf("ambiguous scope mutated registration/evidence: before=%+v after=%+v", before, after)
			}
		})
	}
}
