package nativecodec

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestCodexCaptureGatePrecedesPrivacyOmission(t *testing.T) {
	t.Parallel()
	const id = "00000000-0000-0000-0000-000000000002"
	const other = "00000000-0000-0000-0000-000000000001"
	for _, fields := range []string{
		`,"session_id":"` + other + `"`, `,"parent_thread_id":"` + other + `"`, `,"forked_from_id":"` + other + `"`,
		`,"history_base":{"thread_id":"` + other + `","end_ordinal_exclusive":0,"end_byte_offset":0}`,
		`,"source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + other + `","depth":1}}}`,
	} {
		raw := `{"type":"session_meta","payload":{"id":"` + id + `"` + fields + `}}` + "\n" + `{"type":"response_item","payload":{"type":"message","role":"user","content":"private inherited prompt"}}` + "\n"
		filtered, err := FilterCodexCaptureJSONL(strings.NewReader(raw))
		if !errors.Is(err, archive.ErrRelatedHistory) || len(filtered.Records) != 0 {
			t.Fatalf("partial history admitted: %v", err)
		}
	}
	// Existing retained sources have already lost producer/relationship fields;
	// they still pass the historical privacy-only refilter, byte for byte.
	raw := `{"type":"session_meta","payload":{"id":"` + id + `","session_id":"` + id + `","cwd":"/synthetic/project"}}` + "\n"
	live, err := FilterCodexCaptureJSONL(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	retained, err := (CodexAdapter{}).FilterJSONL(strings.NewReader(raw))
	if err != nil || !reflect.DeepEqual(live.Records, retained.Records) {
		t.Fatalf("ordinary output changed: %v", err)
	}
}

func TestCodexParserUsesThreadIdentityInsteadOfRootIdentity(t *testing.T) {
	t.Parallel()
	for _, root := range []any{"root", "parent", nil} {
		f := archive.NativeFacts{}
		collectFacts(&f, archive.SourceBundle{NativeSessionID: "child"}, map[string]any{"type": "session_meta", "payload": map[string]any{"id": "child", "session_id": root}}, profileCodex)
		if f.IdentityConflict {
			t.Fatalf("root %v conflicts with child thread", root)
		}
	}
	f := archive.NativeFacts{}
	collectFacts(&f, archive.SourceBundle{NativeSessionID: "child"}, map[string]any{"type": "session_meta", "payload": map[string]any{"id": "different", "session_id": "child"}}, profileCodex)
	if !f.IdentityConflict {
		t.Fatal("actual thread mismatch accepted")
	}
}
