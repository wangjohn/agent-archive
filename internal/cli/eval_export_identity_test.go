package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourceio"
)

type evalOpaqueFilter struct{ agentapi.TranscriptFilter }

func (evalOpaqueFilter) Name() string { return string(syntheticID) }

func (evalOpaqueFilter) Filter(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext) (archive.FilteredTranscript, error) {
	return sourceio.FilterJSONL(ctx, in, c, func(r io.Reader) (archive.FilteredTranscript, error) {
		var record struct {
			Conversation string `json:"conversation_token"`
			Speaker      string `json:"speaker"`
		}
		if err := json.NewDecoder(r).Decode(&record); err != nil {
			return archive.FilteredTranscript{}, err
		}
		if record.Conversation != "opaque:fourth/42" || record.Speaker != "customer" {
			return archive.FilteredTranscript{}, fmt.Errorf("unexpected fourth format")
		}
		safe := []byte(`{"conversation_token":"opaque:fourth/42","speaker":"customer"}`)
		return archive.FilteredTranscript{Format: "fourth-native", LocalIdentity: archive.NativeSessionIdentity{ID: record.Conversation}, Records: [][]byte{safe}, Boundary: archive.CaptureBoundary{RetainedRecords: 1, RetainedBytes: len(safe)}}, nil
	})
}

type evalOpaqueParser struct{ calls atomic.Int32 }

func (*evalOpaqueParser) Version() string { return "opaque-1" }

func (p *evalOpaqueParser) Parse(ctx context.Context, b archive.SourceBundle) (archive.Analysis, error) {
	p.calls.Add(1)
	if b.NativeSessionID != "opaque:fourth/42" {
		return archive.Analysis{}, fmt.Errorf("wrong fourth identity: %s", b.NativeSessionID)
	}
	return (&evalAnalysisParser{}).Parse(ctx, b)
}

// Run must accept an integration's sanitized identity without interpreting its
// record fields. Both detail modes select and call its parser exactly once.
func TestEvalExportOpaqueFourthIdentity(t *testing.T) {
	t.Parallel()
	parser := &evalOpaqueParser{}
	base := evalAnalysisRegistry(t, parser)
	var bindings []builtin.Integration
	for _, d := range base.Catalog().All() {
		b, _ := base.Lookup(string(d.ID))
		b.Descriptor = agentmeta.Descriptor{ID: d.ID}
		if d.ID == syntheticID {
			b.Filter = evalOpaqueFilter{b.Filter}
		}
		bindings = append(bindings, b)
	}
	registry, err := builtin.New(base.Catalog(), bindings)
	if err != nil {
		t.Fatal(err)
	}
	f := localEvalFixture(t)
	f.env.Agents = registry
	path := filepath.Join(f.root, "filename-is-not-native.jsonl")
	if err := os.WriteFile(path, []byte(`{"conversation_token":"opaque:fourth/42","speaker":"customer"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, detail := range []string{"metadata", "full"} {
		records, stderr, code := f.evalLines(t, "", "--file", path, "--harness", "synthetic", "--detail", detail)
		if code != 0 || len(records) != 1 || records[0]["native_session_id"] != "opaque:fourth/42" || records[0]["session_id"] != "opaque:fourth/42" || records[0]["transcript_path"] != path {
			t.Fatalf("%s: code %d records %v stderr %s", detail, code, records, stderr)
		}
	}
	if parser.calls.Load() != 2 {
		t.Fatalf("parser calls %d", parser.calls.Load())
	}
}

// Generic filenames keep their spelling; Codex alone owns its rollout convention.
func TestEvalExportLocalIdentityFallbacks(t *testing.T) {
	t.Parallel()
	f := localEvalFixture(t)
	for _, tc := range []struct {
		harness string
		name    string
		raw     string
		want    string
	}{
		{"claude", "Original-Mixed-Case.jsonl", `{"type":"user","message":{"role":"user","content":"inspect"}}`, "Original-Mixed-Case"},
		{"claude", "rollout-time-0a9b3c4d-0000-4000-8000-0000000000aa.jsonl", `{"type":"user","message":{"role":"user","content":"inspect"}}`, "rollout-time-0a9b3c4d-0000-4000-8000-0000000000aa"},
		{"codex", "rollout-time-0a9b3c4d-0000-4000-8000-0000000000aa.jsonl", `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect"}]}}`, "0a9b3c4d-0000-4000-8000-0000000000aa"},
	} {
		path := filepath.Join(f.root, tc.harness, tc.name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(tc.raw+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		records, stderr, code := f.evalLines(t, "", "--file", path, "--harness", tc.harness)
		if code != 0 || len(records) != 1 || records[0]["native_session_id"] != tc.want || records[0]["transcript_path"] != path {
			t.Fatalf("%s: %d %v %s", tc.harness, code, records, stderr)
		}
	}
}

// Text-only scan retains discovery's admitted identity across both detail modes.
func TestEvalExportTextScanPreservesAdmittedIdentity(t *testing.T) {
	t.Parallel()
	f := localEvalFixture(t)
	id := "Cursor-Mixed-Case"
	path := filepath.Join(f.userHome, ".cursor", "projects", cursorSlugFor(filepath.Join(f.userHome, "agent-archive")), "agent-transcripts", id+".txt")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("user: inspect the widget\nassistant: checked it\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	for _, detail := range []string{"metadata", "full"} {
		records, stderr, code := f.evalLines(t, "", "--scan", "--harness", "cursor", "--detail", detail)
		record := recordsBy(records, "transcript_path")[path]
		if code != 0 || record == nil || record["native_session_id"] != id || record["session_id"] != id {
			t.Fatalf("%s: %d %v %s", detail, code, records, stderr)
		}
	}
}
