package cli

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestSetupDetailsUsesUnderlyingTerminalForPager(t *testing.T) {
	t.Parallel()
	for _, interactive := range []bool{true, false} {
		t.Run(map[bool]string{true: "terminal", false: "redirected"}[interactive], func(t *testing.T) {
			t.Parallel()
			var out, paged bytes.Buffer
			env := testEnv(t, t.TempDir(), time.Now())
			env.IsTerminal = func(stream any) bool { return interactive && stream == any(&out) }
			env.LookupEnv = func(name string) (string, bool) { return "test-pager", name == "PAGER" }
			called := false
			env.RunPager = func(_ context.Context, _ string, _ []string, in io.Reader, stdout, _ io.Writer) error {
				called = true
				if stdout != &out {
					t.Fatalf("pager output is %T", stdout)
				}
				_, err := io.Copy(&paged, in)
				return err
			}
			p := newPrompter(strings.NewReader(""), &lockedWriter{w: &out})
			defer p.close()
			p.reviewModel = buildSetupReviewModel(config.Config{}, setupReview{}, time.Now())
			must(t, showSetupReviewDetails(p, env, io.Discard))
			if called != interactive || !strings.Contains(paged.String()+out.String(), "Full settings and privacy") || (interactive && out.Len() != 0) {
				t.Fatalf("called=%v paged=%q output=%q", called, &paged, &out)
			}
		})
	}
}

func TestSetupSpecificWithoutScannerRejectsRetry(t *testing.T) {
	t.Parallel()
	roots := selectorRoots(t, 1)
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("specific\nr\n\n"), &out)
	defer p.close()
	got, err := selectSetupProjects(p, nil, nil, roots, "", "", nil)
	must(t, err)
	if len(got) != 1 || strings.Contains(out.String(), "Retry search") || !strings.Contains(out.String(), "retry is unavailable for this project selection") {
		t.Fatalf("projects=%+v output=%s", got, &out)
	}
}

func TestSetupSpecificDropsOrdinaryNewExclusions(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	plain, parent, child := filepath.Join(base, "plain"), filepath.Join(base, "parent"), filepath.Join(base, "parent", "child")
	imported, excluded, outside := filepath.Join(base, "imported"), filepath.Join(base, "excluded"), filepath.Join(base, "outside")
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var old []archive.ProjectActivation
	for _, root := range []string{plain, parent, child, imported, excluded, outside} {
		old = append(old, archive.ProjectActivation{Root: root, ProjectID: archive.ProjectID(root), Included: root != excluded, ActivatedAt: at})
	}
	candidates := setupProjectCandidates(old[:5], old[:5], nil, "")
	for i := range candidates {
		candidates[i].selected = candidates[i].evidence.Root == parent
	}
	got := applyProjectCandidates(candidates, old, map[string]bool{archive.ProjectID(imported): true})
	rules := map[string]archive.ProjectActivation{}
	for _, rule := range got {
		rules[rule.Root] = rule
	}
	if _, found := rules[plain]; found {
		t.Fatalf("ordinary deselection retained activation: %+v", rules[plain])
	}
	for _, root := range []string{child, imported, excluded} {
		if rule, found := rules[root]; !found || rule.Included || rule.ActivatedAt != at {
			t.Fatalf("required exclusion lost: %s %+v", root, got)
		}
	}
	if !rules[parent].Included || !rules[outside].Included {
		t.Fatalf("selected or outside rule changed: %+v", got)
	}
	again := setupProjectCandidates(nil, got, nil, plain)
	for i := range again {
		if again[i].evidence.Root == plain {
			again[i].selected = true
		}
	}
	reenabled := applyProjectCandidates(again, got, nil)
	found := false
	for _, rule := range reenabled {
		if rule.Root == plain {
			found = true
			if !rule.Included || !rule.ActivatedAt.IsZero() {
				t.Fatalf("reenabled project kept stale activation: %+v", rule)
			}
		}
	}
	if !found {
		t.Fatal("reenabled project missing")
	}
}
