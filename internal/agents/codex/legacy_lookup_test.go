package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

func TestLegacyOrdinaryUnboundKeepsAnchoredSourceAndCurrentToken(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ref := legacyTestFile(t, root, threadA, threadA, map[string]any{})
	ref.Key = threadA
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Revision: "one"}}
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{CodexRollouts: lookup, LegacyUnboundRegistration: true, Policy: transcriptio.OpenPolicy{Root: root, RejectSymlinks: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pass.Close() }()
	snapshot, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	if _, modern := snapshot.(agentapi.SourceAdmissionFacts); modern {
		t.Fatal("legacy evidence fabricated a modern binding")
	}
	validator, ok := snapshot.(agentapi.SourceAdmissionValidator)
	if !ok {
		t.Fatal("actual admission check missing")
	}
	if err := validator.ValidateAdmission(t.Context(), agentapi.SourceAdmission{NativeID: threadA, Cwd: root}); err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateAdmission(t.Context(), agentapi.SourceAdmission{NativeID: threadA, Cwd: t.TempDir()}); agentapi.Failure(err) != agentapi.Unsafe {
		t.Fatal("changed project accepted", err)
	}
	lookup.changed = true
	if err := snapshot.Input().File.Check(); agentapi.Failure(err) != agentapi.Changed {
		t.Fatal("current change ignored", err)
	}
}

func TestLegacyCompatibilityRefusesBoundPaginatedRelatedAndRevert(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"bound", "paginated", "fork", "revert", "base"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			physical := threadA
			extra := map[string]any{}
			switch scenario {
			case "paginated":
				extra["history_mode"] = "paginated"
			case "fork":
				extra["forked_from_id"] = threadB
			case "revert":
				physical = threadB
			case "base":
				extra["history_base"] = map[string]any{"thread_id": threadB, "end_ordinal_exclusive": 1, "end_byte_offset": 1}
			}
			ref := legacyTestFile(t, root, physical, threadA, extra)
			ref.Key = threadA
			lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Revision: "one"}}
			pass, err := (SourceProvider{}).OpenPass(context.Background(), agentapi.SourceEnvironment{CodexRollouts: lookup, LegacyUnboundRegistration: scenario != "bound", Policy: transcriptio.OpenPolicy{Root: root, RejectSymlinks: true}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = pass.Close() }()
			snapshot, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
			if snapshot != nil {
				_ = snapshot.Close()
			}
			if err == nil {
				t.Fatal("modern source flattened")
			}
		})
	}
}

func legacyTestFile(t *testing.T, root, physical, thread string, extra map[string]any) agentapi.SourceRef {
	t.Helper()
	ref, raw := historyFile(t, root, physical, thread, 0, extra, "legacy task")
	if _, present := extra["history_mode"]; !present {
		first, tail, _ := bytes.Cut(raw, []byte("\n"))
		var meta map[string]any
		if err := json.Unmarshal(first, &meta); err != nil {
			t.Fatal(err)
		}
		delete(meta["payload"].(map[string]any), "history_mode")
		first, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ref.Path, append(append(first, '\n'), tail...), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return ref
}
