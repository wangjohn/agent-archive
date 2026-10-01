package collector

import (
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreviewUsesFilteredHeadPromptAndRecentTailName(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "preview.jsonl")
	lines := []string{`{"type":"user","sessionId":"native-session","message":{"role":"user","content":"Fix widget"}}`, `{"type":"user","isMeta":true,"message":{"role":"user","content":"Injected instructions"}}`}
	tool, _ := json.Marshal(map[string]any{"type": "tool_result", "content": strings.Repeat("noise", 10000)})
	lines = append(lines, string(tool), `{"type":"custom-title","customTitle":"Recent widget name"}`, `{"type":"custom-title","customTitle":"unfinished"`)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := transcriptio.Open(transcriptio.OS{}, path, transcriptio.OpenPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	p, err := PreviewTranscript(context.Background(), s, "claude", PreviewLimits{HeadBytes: 1024, TailBytes: 1024, RecordBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Recent widget name" || p.Title != "Fix widget" || p.NameComplete || p.Bytes > 2049 || strings.Contains(p.Title, "Injected") {
		t.Fatalf("preview %+v", p)
	}
}

func TestPreviewDoesNotPromoteTailReplyAcrossUnreadGap(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "preview.jsonl")
	raw := `{"type":"user","message":{"role":"user","content":"<command-name>/review</command-name>"}}` + "\n" + strings.Repeat("x", 5000) + "\n" + `{"type":"assistant","message":{"role":"assistant","content":"Response"}}` + "\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := transcriptio.Open(transcriptio.OS{}, path, transcriptio.OpenPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	p, err := PreviewTranscript(context.Background(), s, "claude", PreviewLimits{HeadBytes: 512, TailBytes: 512, RecordBytes: 512})
	if err != nil || p.Title != "" {
		t.Fatalf("gap inferred prompt %+v %v", p, err)
	}
}
