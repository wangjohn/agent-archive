package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

const threadA = "11111111-1111-4111-8111-111111111111"
const threadB = "22222222-2222-4222-8222-222222222222"
const rolloutC = "33333333-3333-4333-8333-333333333333"
const rolloutD = "44444444-4444-4444-8444-444444444444"

type historyLookup struct {
	thread   agentapi.CodexRolloutSet
	rollouts map[string][]agentapi.SourceRef
	changed  bool
	reads    int
}

func (l *historyLookup) Thread(context.Context, string) (agentapi.CodexRolloutSet, error) {
	l.reads++
	return l.thread, nil
}
func (l *historyLookup) Rollout(_ context.Context, id string) ([]agentapi.SourceRef, error) {
	return l.rollouts[id], nil
}
func (l *historyLookup) Check(context.Context, string, string) error {
	if l.changed {
		return agentapi.Wrap(agentapi.Changed, transcriptio.ErrChanged)
	}
	return nil
}

func historyFile(t testing.TB, dir, id, thread string, start uint64, extra map[string]any, texts ...string) (agentapi.SourceRef, []byte) {
	t.Helper()
	meta := map[string]any{"id": thread, "cwd": dir, "timestamp": "2026-10-01T12:00:00Z", "cli_version": "0.160.0", "originator": "codex_cli_rs", "source": "cli", "history_mode": "paginated"}
	for k, v := range extra {
		meta[k] = v
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	if err := enc.Encode(map[string]any{"type": "session_meta", "ordinal": start, "payload": meta}); err != nil {
		t.Fatal(err)
	}
	for i, text := range texts {
		if err := enc.Encode(map[string]any{"type": "response_item", "ordinal": start + uint64(i) + 1, "timestamp": "2026-10-01T12:01:00Z", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}); err != nil {
			t.Fatal(err)
		}
	}
	ref := agentapi.SourceRef{Path: filepath.Join(dir, "rollout-2026-10-01T12-00-00-"+id+".jsonl")}
	if err := os.WriteFile(ref.Path, out.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return ref, out.Bytes()
}
func historyPass(t testing.TB, root string, l *historyLookup) agentapi.SourcePass {
	t.Helper()
	p, e := (SourceProvider{}).OpenPass(context.Background(), agentapi.SourceEnvironment{CodexRollouts: l, Policy: transcriptio.OpenPolicy{Root: root, RejectSymlinks: true}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := p.Close(); e != nil {
			t.Error(e)
		}
	})
	return p
}
func historyRead(t testing.TB, p agentapi.SourcePass, ref agentapi.SourceRef) archive.SourceBundle {
	t.Helper()
	s, e := p.Read(context.Background(), ref, agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if e := s.Close(); e != nil {
			t.Error(e)
		}
	}()
	f, e := (Filter{}).Filter(context.Background(), s.Input(), agentapi.FilterContext{})
	if e != nil {
		t.Fatal(e)
	}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	reg := archive.SessionRegistration{ArchiveSessionID: "synthetic", NativeSessionID: f.History.ThreadID, ProjectID: "project", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: "codex"}, TranscriptPath: ref.Path, SessionStartedAt: at, RegisteredAt: at}
	b, e := archive.NewSourceBundle(reg, Filter{}, f, at, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e := b.ValidateHistory(); e != nil {
		t.Fatal(e)
	}
	return b
}

func TestRelatedHistoryOwnershipAndRoundTrip(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"fork", "revert", "child_absent", "child_zero", "child_copied"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			base, raw := historyFile(t, dir, threadA, threadA, 0, nil, "ancestor")
			ownThread := threadB
			extra := map[string]any{}
			switch kind {
			case "fork":
				extra["forked_from_id"] = threadA
				extra["forked_from_ordinal_exclusive"] = 2
			case "revert":
				ownThread = threadA
			default:
				extra["parent_thread_id"] = threadA
			}
			var leaf agentapi.SourceRef
			if kind == "child_absent" || kind == "child_zero" || kind == "child_copied" {
				if kind == "child_zero" {
					extra["subagent_history_start_ordinal"] = 0
				}
				if kind == "child_copied" {
					extra["subagent_history_start_ordinal"] = 2
				}
				leaf, _ = historyFile(t, dir, rolloutC, ownThread, 0, extra, "copied-or-owned", "own")
			} else {
				extra["history_base"] = codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}
				leaf, _ = historyFile(t, dir, rolloutC, ownThread, 2, extra, "own")
			}
			l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Revision: "one"}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
			b := historyRead(t, historyPass(t, dir, l), leaf)
			parsed, e := (Parser{}).Parse(t.Context(), b)
			if e != nil {
				t.Fatal(e)
			}
			want := 1
			if kind == "revert" || kind == "child_absent" || kind == "child_zero" {
				want = 2
			}
			if len(parsed.View.Turns) != want {
				t.Fatalf("turns %d want %d: %+v", len(parsed.View.Turns), want, parsed.View.Turns)
			}
			compressed, e := archive.BuildCompressedSource(b)
			if e != nil {
				t.Fatal(e)
			}
			round, e := archive.ReadSourceBundle(bytes.NewReader(compressed.Bytes), archive.DecodeOptions{})
			if e != nil {
				t.Fatal(e)
			}
			filtered, e := (Filter{}).Refilter(t.Context(), round, time.Time{})
			if e != nil {
				t.Fatal(e)
			}
			if len(filtered.Ordinals) != len(b.Ordinals) || len(filtered.History.Spans) != len(b.History.Spans) {
				t.Fatal("refilter lost spans")
			}
		})
	}
}

func TestRelatedSelectionAndDependencyFailures(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"incomplete", "competing", "missing_base", "split_prefix", "ordinal_mismatch", "cycle", "outside", "stale_current"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			base, raw := historyFile(t, dir, threadA, threadA, 0, nil, "before")
			boundary := uint64(len(raw))
			ordinal := uint64(2)
			baseID := threadA
			if kind == "split_prefix" {
				boundary--
			}
			if kind == "ordinal_mismatch" {
				ordinal = 3
			}
			if kind == "cycle" {
				baseID = rolloutC
			}
			leaf, _ := historyFile(t, dir, rolloutC, threadA, ordinal, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: baseID, EndOrdinal: ordinal, EndByteOffset: boundary}}, "after")
			l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Revision: "one"}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
			switch kind {
			case "incomplete":
				l.thread.Current = nil
			case "competing":
				other, _ := historyFile(t, dir, rolloutD, threadA, 0, nil, "other")
				l.thread = agentapi.CodexRolloutSet{Candidates: []agentapi.SourceRef{leaf, other}, Complete: true}
			case "missing_base":
				l.rollouts = nil
			case "outside":
				outside, _ := historyFile(t, t.TempDir(), rolloutD, threadA, 0, nil, "outside")
				l.thread.Current = &outside
			case "stale_current":
				other, _ := historyFile(t, dir, rolloutD, threadB, 0, nil, "wrong")
				l.thread.Current = &other
			}
			p := historyPass(t, dir, l)
			s, e := p.Read(t.Context(), leaf, agentapi.ReadLimits{})
			if e == nil {
				_ = s.Close()
				t.Fatal("accepted invalid history")
			}
		})
	}
}

func TestRelatedSignatureChangesWithAncestorAndCurrentLocator(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, raw := historyFile(t, dir, threadA, threadA, 0, nil, "before")
	leaf, _ := historyFile(t, dir, rolloutC, threadA, 2, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "after")
	l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &base, Revision: "one"}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	p := historyPass(t, dir, l)
	before, e := p.Signature(t.Context(), base)
	if e != nil {
		t.Fatal(e)
	}
	l.thread.Current = &leaf
	l.thread.Revision = "two"
	after, e := p.Signature(t.Context(), base)
	if e != nil {
		t.Fatal(e)
	}
	if before.Signature == after.Signature {
		t.Fatal("ordinary seed bypassed current locator")
	}
	f, e := os.OpenFile(base.Path, os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.WriteString("\n")
	if e = errors.Join(e, f.Close()); e != nil {
		t.Fatal(e)
	}
	changed, e := p.Signature(t.Context(), base)
	if e != nil {
		t.Fatal(e)
	}
	if changed.Signature == after.Signature {
		t.Fatal("ancestor append did not invalidate signature")
	}
}

func TestRelatedCapturedPrefixAcceptsAppendAndRejectsRewrite(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"append", "rewrite", "replace", "locator_change"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			leaf, raw := historyFile(t, dir, threadB, threadB, 0, map[string]any{"parent_thread_id": threadA}, "own")
			l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Revision: "one"}}
			p := historyPass(t, dir, l)
			s, e := p.Read(t.Context(), leaf, agentapi.ReadLimits{})
			if e != nil {
				t.Fatal(e)
			}
			defer func() { _ = s.Close() }()
			switch kind {
			case "append":
				f, e := os.OpenFile(leaf.Path, os.O_APPEND|os.O_WRONLY, 0600)
				if e != nil {
					t.Fatal(e)
				}
				_, e = f.WriteString("{\"ordinal\":2,\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"later\"}}\n")
				if e = errors.Join(e, f.Close()); e != nil {
					t.Fatal(e)
				}
			case "rewrite":
				if e := os.WriteFile(leaf.Path, bytes.Replace(raw, []byte("own"), []byte("new"), 1), 0600); e != nil {
					t.Fatal(e)
				}
			case "replace":
				if e := os.Remove(leaf.Path); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(leaf.Path, raw, 0600); e != nil {
					t.Fatal(e)
				}
			case "locator_change":
				l.changed = true
			}
			out, e := (Filter{}).Filter(t.Context(), s.Input(), agentapi.FilterContext{})
			if kind == "append" {
				if e != nil || len(out.Records) != 2 {
					t.Fatalf("append prefix %d %v", len(out.Records), e)
				}
			} else if agentapi.Failure(e) != agentapi.Changed {
				t.Fatalf("rewrite not retryable: %v", e)
			}
		})
	}
}

func TestRelatedLimitsAndCleanup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	leaf, _ := historyFile(t, dir, threadB, threadB, 0, map[string]any{"parent_thread_id": threadA}, "own")
	l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Revision: "one"}}
	p := historyPass(t, dir, l)
	if _, e := p.Read(t.Context(), leaf, agentapi.ReadLimits{RawBytes: 1}); agentapi.Failure(e) != agentapi.Limit {
		t.Fatalf("byte cap: %v", e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e := p.Read(ctx, leaf, agentapi.ReadLimits{}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if e := p.Close(); e != nil {
		t.Fatal(e)
	}
	owned := p.(*relatedSourcePass)
	if owned.bytes != 0 || len(owned.files) != 0 || len(owned.live) != 0 {
		t.Fatal("pass retained resources")
	}
}

func BenchmarkRelatedHistoryRecords(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			dir := b.TempDir()
			texts := make([]string, n)
			for i := range texts {
				texts[i] = "synthetic prompt"
			}
			leaf, raw := historyFile(b, dir, threadB, threadB, 0, map[string]any{"parent_thread_id": threadA}, texts...)
			l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Revision: "one"}}
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			b.ResetTimer()
			for range b.N {
				p, _ := (SourceProvider{}).OpenPass(context.Background(), agentapi.SourceEnvironment{CodexRollouts: l})
				s, e := p.Read(context.Background(), leaf, agentapi.ReadLimits{})
				if e != nil {
					b.Fatal(e)
				}
				_, e = (Filter{}).Filter(context.Background(), s.Input(), agentapi.FilterContext{})
				if e != nil {
					b.Fatal(e)
				}
				_ = s.Close()
				_ = p.Close()
			}
			b.ReportMetric(float64(len(raw)*3), "native_bytes/op")
			b.ReportMetric(0, "writes/op")
		})
	}
}
