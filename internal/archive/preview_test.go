package archive

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPreviewRecordsShareFilteredLabels(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		harness string
		file    string
	}{{"claude", "claude-session-name.jsonl"}, {"claude", "claude-task-notification.jsonl"}, {"claude", "claude-skill-command.jsonl"}, {"codex", "codex-startup-shell.jsonl"}, {"codex", "codex-safe-and-sensitive.jsonl"}} {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile("testdata/" + tc.file)
			if err != nil {
				t.Fatal(err)
			}
			adapter, _ := NewAdapter(tc.harness)
			filtered, err := adapter.FilterJSONL(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := NewSourceBundle(SessionRegistration{ArchiveSessionID: "preview", NativeSessionID: "preview", ProjectID: "project", ProjectRoot: "/project", SessionStartedAt: time.Now(), Harness: Harness{Name: tc.harness}}, adapter, filtered, time.Now(), nil)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := SessionLabels(bundle)
			var accumulator PreviewAccumulator
			scanner := bufio.NewScanner(bytes.NewReader(raw))
			scanner.Buffer(make([]byte, 64*1024), MaxRecordBytes)
			for scanner.Scan() {
				if e := accumulator.Add(tc.harness, scanner.Bytes(), true); e != nil {
					t.Fatal(e)
				}
			}
			got := accumulator.Labels
			if got.Name != want.Name || got.Title != want.Title || got.Branch != want.Branch {
				t.Fatalf("preview=%+v labels=%+v", got, want)
			}
		})
	}
}

func TestPreviewNeverLabelsInjectedOrSecretFields(t *testing.T) {
	t.Parallel()
	for _, line := range []string{`{"type":"user","isMeta":true,"message":{"role":"user","content":"injected instruction"}}`, `{"type":"user","message":{"role":"system","content":"injected instruction"}}`, `{"type":"custom-title","customTitle":"api_key=sk-123456789012345678901234567890","secret":"injected instruction"}`} {
		got, err := PreviewRecord("claude", []byte(line))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got.Name+got.Title, "injected instruction") || strings.Contains(got.Name+got.Title, "sk-123456789012345678901234567890") {
			t.Fatalf("unsafe label: %+v", got)
		}
	}
}
