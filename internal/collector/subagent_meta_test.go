package collector

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// newNamedSubagentFixture is a resumed-subagent fixture whose transcript is
// named as Claude Code names one, agent-agent-1.jsonl, so a .meta.json can sit
// beside it.
func newNamedSubagentFixture(t *testing.T) *resumedSubagentFixture {
	t.Helper()
	f := newResumedSubagentFixture(t)
	f.childPath = filepath.Join(filepath.Dir(f.childPath), "agent-agent-1.jsonl")
	return f
}

func (f *resumedSubagentFixture) writeMeta(content string) {
	f.t.Helper()
	path, ok := archive.SubagentMetaPath(f.childPath)
	if !ok {
		f.t.Fatalf("%s is not named like a subagent transcript", f.childPath)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// childBundle is the published source bundle of the child.
func (f *resumedSubagentFixture) childBundle() archive.SourceBundle {
	f.t.Helper()
	return fetchBundle(f.t, f.remote, fetchMetadata(f.t, f.remote, "claude", "child"))
}

func descriptionRecords(bundle archive.SourceBundle) []map[string]any {
	var out []map[string]any
	for _, record := range bundle.NativeRecords {
		if record["type"] == "subagent-meta" {
			out = append(out, record)
		}
	}
	return out
}

const syntheticMeta = `{"agentType":"general-purpose","description":"Find the retention tests password=SYNTHETICSUBAGENTPW","worktreePath":"/work/synthetic/.worktrees/agent-1"}`

// The description in a subagent's .meta.json is published with it: the first
// record of its source, redacted, and the name in its metadata. Nothing else of
// the file is.
func TestSubagentDescriptionIsPublishedWithTheSubagent(t *testing.T) {
	f := newNamedSubagentFixture(t)
	f.writeMeta(syntheticMeta)
	f.write(2)
	f.stop(3)
	if result := f.run(4); !slices.Contains(result.Published, "child") {
		t.Fatalf("result=%#v", result)
	}
	metadata := fetchMetadata(t, f.remote, "claude", "child")
	if metadata.Name != "Find the retention tests password=[REDACTED]" {
		t.Fatalf("name = %q", metadata.Name)
	}
	bundle := f.childBundle()
	if len(bundle.NativeRecords) != 2 || bundle.NativeRecords[0]["type"] != "subagent-meta" || len(bundle.NativeRecords[0]) != 2 {
		t.Fatalf("records = %#v, want the subagent-meta record first, then the transcript's one", bundle.NativeRecords)
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"SYNTHETICSUBAGENTPW", "/work/synthetic", "general-purpose", "worktreePath", "agentType"} {
		if bytes.Contains(encoded, []byte(leaked)) {
			t.Errorf("the published source holds %q", leaked)
		}
	}
	if parent := fetchMetadata(t, f.remote, "claude", "parent"); parent.Name != "" {
		t.Errorf("the parent took the subagent's name: %q", parent.Name)
	}
}

// A subagent with no .meta.json, or an unusable one, publishes as it did: no
// description record, no name, and no gap about it.
func TestSubagentWithoutAUsableMetaFileIsPublishedAsBefore(t *testing.T) {
	cases := map[string]func(f *resumedSubagentFixture){
		"missing": func(*resumedSubagentFixture) {},
		"malformed": func(f *resumedSubagentFixture) {
			f.writeMeta(`{"description": "cut off`)
		},
		"blank": func(f *resumedSubagentFixture) {
			f.writeMeta(`{"description":"  "}`)
		},
		"not text": func(f *resumedSubagentFixture) {
			f.writeMeta(`{"description":["SYNTHETIC-LIST"]}`)
		},
		"oversized": func(f *resumedSubagentFixture) {
			f.writeMeta(`{"description":"Too big","pad":"` + strings.Repeat("x", archive.MaxSubagentMetaBytes) + `"}`)
		},
		"a directory": func(f *resumedSubagentFixture) {
			path, _ := archive.SubagentMetaPath(f.childPath)
			if err := os.Mkdir(path, 0o700); err != nil {
				f.t.Fatal(err)
			}
		},
		"a pipe": func(f *resumedSubagentFixture) {
			path, _ := archive.SubagentMetaPath(f.childPath)
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				f.t.Skipf("no named pipes here: %v", err)
			}
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			f := newNamedSubagentFixture(t)
			prepare(f)
			f.write(2)
			f.stop(3)
			if result := f.run(4); !slices.Contains(result.Published, "child") {
				t.Fatalf("result=%#v", result)
			}
			metadata := fetchMetadata(t, f.remote, "claude", "child")
			if metadata.Name != "" || len(descriptionRecords(f.childBundle())) != 0 {
				t.Fatalf("name %q, records %#v", metadata.Name, f.childBundle().NativeRecords)
			}
			for _, gap := range metadata.CaptureGaps {
				if strings.Contains(gap.Detail, "description") || strings.Contains(gap.Detail, "meta") || gap.Code == "unsupported_value_omitted" {
					t.Errorf("a gap names the optional file: %#v", gap)
				}
			}
		})
	}
}

// A .meta.json written after the subagent was captured does not stop its
// later snapshots: when the transcript next changes, the description is
// published, and the earlier snapshot, which had none, is extended by it (the
// description is not evidence of the transcript), not blocked as rewritten.
func TestSubagentMetaFileThatAppearsLaterIsPublishedWithTheNextChange(t *testing.T) {
	f := newNamedSubagentFixture(t)
	f.write(2)
	f.stop(3)
	if result := f.run(4); !slices.Contains(result.Published, "child") {
		t.Fatalf("first result=%#v", result)
	}
	if f.messages() != 1 || fetchMetadata(t, f.remote, "claude", "child").Name != "" {
		t.Fatal("the first snapshot has a name")
	}

	// Nothing about the transcript changed, so the pass does not read it, and
	// the file that is there now is not noticed.
	f.writeMeta(syntheticMeta)
	result := f.run(5)
	if slices.Contains(result.Published, "child") || !slices.Contains(result.Skipped, "child") {
		t.Fatalf("an unchanged transcript was read again: %#v", result)
	}
	if fetchMetadata(t, f.remote, "claude", "child").Name != "" {
		t.Fatal("a .meta.json beside an unchanged transcript was published")
	}

	// The transcript changes: the file is read, and the snapshot that had no
	// description is extended by the one that has.
	f.write(6)
	f.stop(7)
	if result := f.run(8); !slices.Contains(result.Published, "child") {
		t.Fatalf("second result=%#v", result)
	}
	metadata := fetchMetadata(t, f.remote, "claude", "child")
	if f.messages() != 2 || metadata.Name != "Find the retention tests password=[REDACTED]" {
		t.Fatalf("messages %d, name %q", f.messages(), metadata.Name)
	}
	for _, gap := range metadata.CaptureGaps {
		if gap.Code == "transcript_rewritten" {
			t.Errorf("a new description was taken for a rewrite: %#v", gap)
		}
	}
}

// A description that changes between snapshots is the latest one, again
// without counting as a rewrite.
func TestSubagentDescriptionChangeIsNotARewrite(t *testing.T) {
	f := newNamedSubagentFixture(t)
	f.writeMeta(`{"description":"First name"}`)
	f.write(2)
	f.stop(3)
	f.run(4)
	if got := fetchMetadata(t, f.remote, "claude", "child").Name; got != "First name" {
		t.Fatalf("name = %q", got)
	}
	f.writeMeta(`{"description":"Second name"}`)
	f.write(5)
	f.stop(6)
	if result := f.run(7); !slices.Contains(result.Published, "child") {
		t.Fatalf("result=%#v", result)
	}
	if got := fetchMetadata(t, f.remote, "claude", "child").Name; got != "Second name" || f.messages() != 2 {
		t.Fatalf("name = %q, messages %d", got, f.messages())
	}
	if records := descriptionRecords(f.childBundle()); len(records) != 1 {
		t.Fatalf("records = %#v", records)
	}
}

// An imported subagent (backfill registers its transcript as a candidate with
// an imported parent) is filtered with its .meta.json like a hook-reported
// one: the collector's registration carries the parent either way.
func TestImportedSubagentIsPublishedWithItsDescription(t *testing.T) {
	f := newNamedSubagentFixture(t)
	f.writeMeta(syntheticMeta)
	f.write(2)
	parent, found, err := f.local.LoadRegistration("parent")
	if err != nil || !found {
		t.Fatal(err)
	}
	candidate := state.SubagentCandidate{
		ArchiveSessionID: "child", NativeSessionID: "parent-native:subagent:agent-1", ParentArchiveSessionID: "parent",
		ParentNativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project",
		Harness: archive.Harness{Name: "claude"}, AgentID: "agent-1", TranscriptPath: f.childPath,
		ObservedAt: f.start.Add(3 * time.Minute), Origin: archive.SessionOriginImport,
	}
	reg := assembleSubagentRegistration(parent, candidate)
	if reg.ParentSessionID != "parent" || reg.TranscriptPath != f.childPath {
		t.Fatalf("registration = %+v", reg)
	}
	filtered, _, err := filterTranscript(archive.ClaudeAdapter{}, reg, DefaultMaxTranscriptBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Records) != 2 || !bytes.HasPrefix(filtered.Records[0], []byte(`{"description":"Find the retention tests password=[REDACTED]","type":"subagent-meta"}`)) {
		t.Fatalf("records = %q", filtered.Records)
	}
}

// Only a subagent's transcript reads a .meta.json: not a session without a
// parent, even one named like a subagent's with a file beside it, and not a
// subagent whose transcript is named otherwise.
func TestOnlyASubagentTranscriptReadsAMetaFile(t *testing.T) {
	dir := t.TempDir()
	line := `{"type":"assistant","sessionId":"parent-native","agentId":"agent-1","timestamp":"2026-09-21T10:02:00Z","message":{"role":"assistant","content":"child"}}` + "\n"
	agentPath := writeTranscript(t, dir, "agent-1.jsonl", line)
	otherPath := writeTranscript(t, dir, "child.jsonl", line)
	for _, name := range []string{"agent-1.meta.json", "child.meta.json"} {
		writeTranscript(t, dir, name, `{"description":"SYNTHETIC-MUST-NOT-APPEAR"}`)
	}
	for name, reg := range map[string]archive.SessionRegistration{
		"top-level session named like a subagent": {TranscriptPath: agentPath},
		"top-level session":                       {TranscriptPath: otherPath},
		"subagent named otherwise":                {TranscriptPath: otherPath, ParentSessionID: "parent", SubagentID: "agent-1"},
	} {
		reg.Harness = archive.Harness{Name: "claude"}
		before := subagentMetaReads.Load()
		filtered, _, err := filterTranscript(archive.ClaudeAdapter{}, reg, DefaultMaxTranscriptBytes)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if reads := subagentMetaReads.Load() - before; reads != 0 {
			t.Errorf("%s: read %d meta files", name, reads)
		}
		if bytes.Contains(bytes.Join(filtered.Records, nil), []byte("SYNTHETIC-MUST-NOT-APPEAR")) || len(filtered.Records) != 1 {
			t.Errorf("%s: records = %q", name, filtered.Records)
		}
	}
	// A subagent does read it, and no other adapter does.
	reg := archive.SessionRegistration{TranscriptPath: agentPath, ParentSessionID: "parent", SubagentID: "agent-1", Harness: archive.Harness{Name: "claude"}}
	before := subagentMetaReads.Load()
	filtered, _, err := filterTranscript(archive.ClaudeAdapter{}, reg, DefaultMaxTranscriptBytes)
	if err != nil || subagentMetaReads.Load()-before != 1 || len(filtered.Records) != 2 {
		t.Fatalf("subagent: reads %d, records %q, err %v", subagentMetaReads.Load()-before, filtered.Records, err)
	}
	reg.Harness = archive.Harness{Name: "codex"}
	before = subagentMetaReads.Load()
	if _, _, err := filterTranscript(archive.CodexAdapter{}, reg, DefaultMaxTranscriptBytes); err == nil || subagentMetaReads.Load() != before {
		t.Fatalf("a Codex transcript read a meta file (reads %d, err %v)", subagentMetaReads.Load()-before, err)
	}
}

// An unusable file changes nothing about what the filter returns.
func TestUnusableMetaFileLeavesTheFilteredTranscriptUnchanged(t *testing.T) {
	dir := t.TempDir()
	line := `{"type":"assistant","sessionId":"parent-native","agentId":"agent-1","timestamp":"2026-09-21T10:02:00Z","message":{"role":"assistant","content":"child"}}` + "\n"
	path := writeTranscript(t, dir, "agent-1.jsonl", line)
	reg := archive.SessionRegistration{TranscriptPath: path, ParentSessionID: "parent", SubagentID: "agent-1", Harness: archive.Harness{Name: "claude"}}
	want, _, err := filterTranscript(archive.ClaudeAdapter{}, reg, DefaultMaxTranscriptBytes)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"malformed": "{", "empty": "", "no description": `{"agentType":"x"}`, "blank": `{"description":" "}`} {
		writeTranscript(t, dir, "agent-1.meta.json", content)
		got, _, err := filterTranscript(archive.ClaudeAdapter{}, reg, DefaultMaxTranscriptBytes)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: filtered differently (err %v): %+v vs %+v", name, err, got, want)
		}
	}
}

// A subagent-meta record is a label, not evidence: a snapshot with it, without
// it, or with another description extends one that has any other, but a
// transcript that lost a record does not extend one that had it.
func TestNativeEvidenceExtendsIgnoresTheSubagentDescription(t *testing.T) {
	t.Parallel()
	bundle := func(records ...map[string]any) archive.SourceBundle {
		var b archive.SourceBundle
		b.Capture.SourceFormat = "claude-jsonl"
		b.Capture.FilterVersion, b.Capture.AdapterVersion = "14", "0.14.0"
		b.NativeRecords = records
		return b
	}
	first := map[string]any{"type": "user", "uuid": "u1"}
	second := map[string]any{"type": "assistant", "uuid": "a1"}
	named := map[string]any{"type": "subagent-meta", "description": "One"}
	renamed := map[string]any{"type": "subagent-meta", "description": "Two"}
	for name, c := range map[string]struct {
		previous, candidate archive.SourceBundle
		want                bool
	}{
		"gained a description":  {bundle(first), bundle(named, first, second), true},
		"changed description":   {bundle(named, first), bundle(renamed, first, second), true},
		"lost the description":  {bundle(named, first), bundle(first, second), true},
		"lost a record":         {bundle(named, first, second), bundle(named, first), false},
		"record replaced":       {bundle(named, first), bundle(named, second), false},
		"description not first": {bundle(first, named), bundle(first, renamed), false},
	} {
		if got := nativeEvidenceExtends(c.previous, c.candidate); got != c.want {
			t.Errorf("%s: extends = %v, want %v", name, got, c.want)
		}
	}
}
