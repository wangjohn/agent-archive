package codex

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

type headerMutationLookup struct {
	historyLookup
	onThread  func()
	onRollout func(string)
}

func (l *headerMutationLookup) Thread(ctx context.Context, id string) (agentapi.CodexRolloutSet, error) {
	if l.onThread != nil {
		f := l.onThread
		l.onThread = nil
		f()
	}
	return l.historyLookup.Thread(ctx, id)
}

func (l *headerMutationLookup) Rollout(ctx context.Context, id string) ([]agentapi.SourceRef, error) {
	if l.onRollout != nil {
		l.onRollout(id)
	}
	return l.historyLookup.Rollout(ctx, id)
}

func rewriteHeader(t *testing.T, ref agentapi.SourceRef, raw []byte, old, replacement string) {
	t.Helper()
	changed := bytes.Replace(raw, []byte(old), []byte(replacement), 1)
	if bytes.Equal(raw, changed) || len(raw) != len(changed) {
		t.Fatal("expected same-length header mutation")
	}
	if err := os.WriteFile(ref.Path, changed, 0600); err != nil {
		t.Fatal(err)
	}
}

func mutationPass(t *testing.T, dir string, lookup *headerMutationLookup) *relatedSourcePass {
	t.Helper()
	p, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{CodexRollouts: lookup, Policy: transcriptio.OpenPolicy{Root: dir, RejectSymlinks: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	return p.(*relatedSourcePass)
}

func TestHeaderRewriteDuringGraphDefersCaptureAndRetries(t *testing.T) {
	for _, field := range []struct {
		name        string
		old         string
		replacement string
	}{
		{"ownership", `"subagent_history_start_ordinal":0`, `"subagent_history_start_ordinal":2`},
		{"thread", threadB, rolloutD},
		{"project", `"cwd":"`, `"cwd":"`},
		{"mode", `"history_mode":"paginated"`, `"history_mode":"unknownxx"`},
		{"graph", threadA, rolloutD},
	} {
		t.Run(field.name, func(t *testing.T) {
			dir := t.TempDir()
			base, raw := historyFile(t, dir, threadA, threadB, 0, nil, "copied")
			leaf, leafRaw := historyFile(t, dir, rolloutC, threadB, 2, map[string]any{"parent_thread_id": threadA, "subagent_history_start_ordinal": 0, "history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "own")
			lookup := &headerMutationLookup{historyLookup: historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}}
			lookup.onRollout = func(string) {
				lookup.onRollout = nil
				old, replacement := field.old, field.replacement
				if field.name == "project" {
					old = dir
					replacement = dir[:len(dir)-1] + "x"
				}
				rewriteHeader(t, leaf, leafRaw, old, replacement)
			}
			p := mutationPass(t, dir, lookup)
			snapshot, err := p.Read(t.Context(), leaf, agentapi.ReadLimits{})
			if snapshot != nil {
				closeHeaderResource(t, snapshot)
			}
			if !agentapi.HasFailure(err, agentapi.Changed) {
				t.Fatalf("header rewrite accepted: %v", err)
			}
			if p.bytes != 0 || len(p.files) != 0 || len(p.live) != 0 {
				t.Fatal("failed read retained resources")
			}
			if err := os.WriteFile(leaf.Path, leafRaw, 0600); err != nil {
				t.Fatal(err)
			}
			snapshot, err = p.Read(t.Context(), leaf, agentapi.ReadLimits{})
			if err != nil {
				t.Fatalf("settled retry: %v", err)
			}
			if _, err := (Filter{}).Filter(t.Context(), snapshot.Input(), agentapi.FilterContext{}); err != nil {
				t.Fatal(err)
			}
			closeHeaderResource(t, snapshot)
			other, _ := historyFile(t, dir, rolloutD, rolloutD, 0, nil, "unrelated")
			lookup.thread.Current = &other
			snapshot, err = p.Read(t.Context(), other, agentapi.ReadLimits{})
			if err != nil {
				t.Fatalf("unrelated progress: %v", err)
			}
			closeHeaderResource(t, snapshot)
		})
	}
}

func TestDependencyHeaderRewriteDuringGraphDefersCapture(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "included", true: "zero_prefix"}[empty], func(t *testing.T) {
			dir := t.TempDir()
			root, rootRaw := historyFile(t, dir, threadA, threadB, 0, nil, "root")
			start, rootEnd := uint64(2), uint64(len(rootRaw))
			if empty {
				start, rootEnd = 0, 0
			}
			base, baseRaw := historyFile(t, dir, rolloutD, threadB, start, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: start, EndByteOffset: rootEnd}}, "base")
			leafStart, baseEnd := start+2, uint64(len(baseRaw))
			if empty {
				leafStart, baseEnd = 0, 0
			}
			leaf, _ := historyFile(t, dir, rolloutC, threadB, leafStart, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: rolloutD, EndOrdinal: leafStart, EndByteOffset: baseEnd}}, "own")
			lookup := &headerMutationLookup{historyLookup: historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}, rollouts: map[string][]agentapi.SourceRef{threadA: {root}, rolloutD: {base}}}}
			lookup.onRollout = func(id string) {
				if id == threadA {
					lookup.onRollout = nil
					rewriteHeader(t, base, baseRaw, `"history_mode":"paginated"`, `"history_mode":"unknownxx"`)
				}
			}
			p := mutationPass(t, dir, lookup)
			snapshot, err := p.Read(t.Context(), leaf, agentapi.ReadLimits{})
			if snapshot != nil {
				closeHeaderResource(t, snapshot)
			}
			if !agentapi.HasFailure(err, agentapi.Changed) {
				t.Fatalf("dependency rewrite accepted: %v", err)
			}
		})
	}
}

func TestSelectionHintHeaderProofAllowsAppendAndRejectsRewrite(t *testing.T) {
	for _, appendOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "rewrite", true: "append"}[appendOnly], func(t *testing.T) {
			dir := t.TempDir()
			hint, raw := historyFile(t, dir, threadB, threadB, 0, nil, "previous")
			leaf, _ := historyFile(t, dir, rolloutC, threadB, 0, nil, "current")
			lookup := &headerMutationLookup{historyLookup: historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}}, onThread: func() {
				if appendOnly {
					if err := os.WriteFile(hint.Path, append(bytes.Clone(raw), []byte("{}\n")...), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					rewriteHeader(t, hint, raw, threadB, rolloutD)
				}
			},
			}
			p := mutationPass(t, dir, lookup)
			snapshot, err := p.Read(t.Context(), hint, agentapi.ReadLimits{})
			if !appendOnly {
				if snapshot != nil {
					closeHeaderResource(t, snapshot)
				}
				if !agentapi.HasFailure(err, agentapi.Changed) {
					t.Fatalf("selection header rewrite accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("selection hint append blocked progress: %v", err)
			}
			defer closeHeaderResource(t, snapshot)
			// A selection-only descriptor must stay leased until the native proof ends.
			if err := p.evict(); err != nil {
				t.Fatal(err)
			}
			if _, err := (Filter{}).Filter(t.Context(), snapshot.Input(), agentapi.FilterContext{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestZeroPrefixDependencyHeaderRemainsBound(t *testing.T) {
	for _, admission := range []bool{false, true} {
		t.Run(map[bool]string{false: "filter", true: "admission"}[admission], func(t *testing.T) {
			dir := t.TempDir()
			base, baseRaw := historyFile(t, dir, threadA, threadB, 0, nil, "excluded")
			leaf, _ := historyFile(t, dir, rolloutC, threadB, 0, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA}}, "own")
			lookup := &headerMutationLookup{historyLookup: historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}}
			p := mutationPass(t, dir, lookup)
			snapshot, err := p.Read(t.Context(), leaf, agentapi.ReadLimits{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeHeaderResource(t, snapshot)
			rewriteHeader(t, base, baseRaw, threadB, rolloutD)
			if admission {
				err = snapshot.(agentapi.SourceAdmissionValidator).ValidateAdmission(t.Context(), agentapi.SourceAdmission{NativeID: threadB, Cwd: dir})
			} else {
				_, err = (Filter{}).Filter(t.Context(), snapshot.Input(), agentapi.FilterContext{})
			}
			if !agentapi.HasFailure(err, agentapi.Changed) {
				t.Fatalf("zero-prefix header rewrite accepted: %v", err)
			}
		})
	}
}

// headerBoundaryOpener rewrites after the initial metadata read, during the
// source's last-record boundary read, before its captured prefix is hashed.
type headerBoundaryOpener struct {
	transcriptio.OS
	offset int64
	mutate func()
}

func (o *headerBoundaryOpener) OpenRegular(path string) (transcriptio.File, error) {
	f, err := o.OS.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	return headerBoundaryFile{File: f, opener: o}, nil
}

type headerBoundaryFile struct {
	transcriptio.File
	opener *headerBoundaryOpener
}

func (f headerBoundaryFile) ReadAt(p []byte, off int64) (int, error) {
	if off == f.opener.offset && f.opener.mutate != nil {
		mutate := f.opener.mutate
		f.opener.mutate = nil
		mutate()
	}
	return f.File.ReadAt(p, off)
}

func TestOrdinaryHeaderRewriteBeforePrefixHashDefersCapture(t *testing.T) {
	for _, signature := range []bool{false, true} {
		t.Run(map[bool]string{false: "read_admission", true: "signature"}[signature], func(t *testing.T) {
			dir := t.TempDir()
			leaf, raw := historyFile(t, dir, threadA, threadA, 0, nil, "own")
			files := &headerBoundaryOpener{offset: int64(len(raw) - 1), mutate: func() { rewriteHeader(t, leaf, raw, dir, dir[:len(dir)-1]+"x") }}
			p, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{Files: files, Policy: transcriptio.OpenPolicy{Root: dir, RejectSymlinks: true}})
			if err != nil {
				t.Fatal(err)
			}
			defer closeHeaderResource(t, p)
			if signature {
				_, err = p.Signature(t.Context(), leaf)
			} else {
				var snapshot agentapi.SourceSnapshot
				snapshot, err = p.Read(t.Context(), leaf, agentapi.ReadLimits{})
				if snapshot != nil {
					defer closeHeaderResource(t, snapshot)
					err = snapshot.(agentapi.SourceAdmissionValidator).ValidateAdmission(t.Context(), agentapi.SourceAdmission{NativeID: threadA, Cwd: dir})
				}
			}
			if !agentapi.HasFailure(err, agentapi.Changed) {
				t.Fatalf("ordinary stale admission header accepted: %v", err)
			}
			if !signature {
				related := p.(*relatedSourcePass)
				if related.bytes != 0 || len(related.files) != 0 {
					t.Fatal("ordinary failed read retained resources")
				}
			}
		})
	}
}

func closeHeaderResource(t *testing.T, resource io.Closer) {
	t.Helper()
	if err := resource.Close(); err != nil {
		t.Error(err)
	}
}
