package nativecodec

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	"strings"
	"testing"
)

func TestClaudeNativeTitlePrecedenceAndOwnership(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"generated", []string{`{"type":"ai-title","aiTitle":"First","sessionId":"s"}`, `{"type":"ai-title","aiTitle":"Latest","sessionId":"s"}`}, "Latest"},
		{"custom before generated", []string{`{"type":"custom-title","customTitle":"Chosen","sessionId":"s"}`, `{"type":"ai-title","aiTitle":"Generated","sessionId":"s"}`}, "Chosen"},
		{"latest custom", []string{`{"type":"ai-title","aiTitle":"Generated","sessionId":"s"}`, `{"type":"custom-title","customTitle":"Old","sessionId":"s"}`, `{"type":"custom-title","customTitle":"Chosen","sessionId":"s"}`}, "Chosen"},
		{"fork prefix", []string{`{"type":"ai-title","aiTitle":"Own","sessionId":"s"}`, `{"type":"custom-title","customTitle":"Parent","sessionId":"parent"}`}, "Own"},
		{"sidechain", []string{`{"type":"ai-title","aiTitle":"Own","sessionId":"s"}`, `{"type":"custom-title","customTitle":"Child","sessionId":"s","isSidechain":true}`}, "Own"},
		{"legacy", []string{`{"type":"custom-title","customTitle":"Legacy"}`, `{"type":"ai-title","aiTitle":"Generated"}`}, "Legacy"},
		{"malformed ID", []string{`{"type":"ai-title","aiTitle":"Own","sessionId":"s"}`, `{"type":"custom-title","customTitle":"Bad","sessionId":42}`}, "Own"},
		{"empty ID", []string{`{"type":"ai-title","aiTitle":"Own","sessionId":"s"}`, `{"type":"custom-title","customTitle":"Bad","sessionId":""}`}, "Own"},
		{"malformed sidechain", []string{`{"type":"ai-title","aiTitle":"Own","sessionId":"s"}`, `{"type":"custom-title","customTitle":"Bad","sessionId":"s","isSidechain":"true"}`}, "Own"},
		{"filtered ID", []string{`{"type":"ai-title","aiTitle":"Own","sessionId":"s"}`, `{"type":"custom-title","customTitle":"Bad","sessionId":"<system-reminder>hidden</system-reminder>"}`}, "Own"},
		{"invalid latest", []string{`{"type":"ai-title","aiTitle":"Own","sessionId":"s"}`, `{"type":"ai-title","aiTitle":42}`}, "Own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bundle := claudeLabelLines(t, tc.lines...)
			labels := labelsOf(t, bundle)
			if labels.Name != tc.want || labels.Title != "Rename the widget parser" {
				t.Fatalf("labels %+v", labels)
			}
			previews := archive.PreviewAccumulator{NativeID: "s"}
			for _, line := range tc.lines {
				facts, err := PreviewRecord("claude", []byte(line))
				if err != nil {
					t.Fatal(err)
				}
				previews.AddFacts(facts, true)
			}
			if previews.Labels.Name != tc.want {
				t.Fatalf("preview %+v", previews.Labels)
			}
		})
	}
}

func TestClaudeNameBookkeepingDoesNotChangeActivityOrAdmission(t *testing.T) {
	t.Parallel()
	before := claudeLabelLines(t)
	after := claudeLabelLines(t, `{"type":"ai-title","aiTitle":"Generated","sessionId":"s","timestamp":"2099-01-01T00:00:00Z"}`, `{"type":"custom-title","customTitle":"Chosen","timestamp":"1900-01-01T00:00:00Z"}`)
	old, err := ParseClaude(context.Background(), before)
	if err != nil {
		t.Fatal(err)
	}
	next, err := ParseClaude(context.Background(), after)
	if err != nil {
		t.Fatal(err)
	}
	if !old.View.LatestRecordAt.Equal(next.View.LatestRecordAt) || !old.Facts.EarliestRecordAt.Equal(next.Facts.EarliestRecordAt) {
		t.Fatal("title changed activity")
	}
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(`{"type":"ai-title","aiTitle":"Only a name","sessionId":"s","timestamp":"2099-01-01T00:00:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !filtered.NativeStartAt.IsZero() || !filtered.NativeEndAt.IsZero() || !filtered.FirstEventAt.IsZero() || len(filtered.SessionIDs) != 0 || len(filtered.LocalIdentity.Candidates) != 0 {
		t.Fatalf("name established admission: %+v", filtered)
	}
	stub := claudeLines(t, `{"type":"custom-title","customTitle":"Stub"}`)
	if _, ok := SessionLabels(stub); ok {
		t.Fatal("title-only stub eligible")
	}
}

func TestClaudeAITitleIsTypedAndRedacted(t *testing.T) {
	t.Parallel()
	input := `{"type":"ai-title","aiTitle":"password=hunter2","sessionId":"s","lastPrompt":"hidden","unknown":"hidden"}`
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(filtered.Records)
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "hidden") {
		t.Fatalf("unsafe %s", raw)
	}
	facts, err := PreviewRecord("claude", []byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if facts.Name != "password=[REDACTED]" || !facts.Activity.IsZero() {
		t.Fatalf("facts %+v", facts)
	}
	for _, input := range []string{`{"type":"user","aiTitle":"Wrong","message":{"role":"user","content":"Prompt"}}`, `{"type":"ai-title","aiTitle":{"nested":"Wrong"}}`, `{"type":"ai-title","aiTitle":" "}`} {
		f, err := PreviewRecord("claude", []byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if f.Name != "" {
			t.Fatalf("untyped name %+v", f)
		}
	}
}

func TestCodexOpenPageContextIsNotAPromptFallback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"context only", "<external_codex_apps_open_page>Selected page</external_codex_apps_open_page>", ""},
		{"mixed", "<external_codex_apps_open_page>Selected page</external_codex_apps_open_page>Review this page.", "Review this page."},
		{"quoted markup", "Why does `<external_codex_apps_open_page>...</external_codex_apps_open_page>` appear?", "Why does `<external_codex_apps_open_page>...</external_codex_apps_open_page>` appear?"},
		{"indented code", "    <external_codex_apps_open_page>example</external_codex_apps_open_page>", "<external_codex_apps_open_page>example</external_codex_apps_open_page>"},
		{"indented suffix", "<external_codex_apps_open_page>Context</external_codex_apps_open_page>\n    <external_codex_apps_open_page>example</external_codex_apps_open_page>", "<external_codex_apps_open_page>example</external_codex_apps_open_page>"},
		{"indented example with other injected context", "    <external_codex_apps_open_page>example</external_codex_apps_open_page>\n<system-reminder>Injected</system-reminder>", "<external_codex_apps_open_page>example</external_codex_apps_open_page>"},
		{"whitespace blank before indented code", " \n    <external_codex_apps_open_page>example</external_codex_apps_open_page>", "<external_codex_apps_open_page>example</external_codex_apps_open_page>"},
		{"whitespace blank after native context", "<external_codex_apps_open_page>Context</external_codex_apps_open_page> \n    <external_codex_apps_open_page>example</external_codex_apps_open_page>", "<external_codex_apps_open_page>example</external_codex_apps_open_page>"},
		{"whitespace blank after reminder", "<system-reminder>Injected</system-reminder> \n    <external_codex_apps_open_page>example</external_codex_apps_open_page>", "<external_codex_apps_open_page>example</external_codex_apps_open_page>"},
		{"tab blank and code after both wrappers", "<system-reminder>Injected</system-reminder><external_codex_apps_open_page>Context</external_codex_apps_open_page> \t\r\n\t<external_codex_apps_open_page>example</external_codex_apps_open_page>", "<external_codex_apps_open_page>example</external_codex_apps_open_page>"},
		{"ordinary example", "Explain external_codex_apps_open_page with examples.", "Explain external_codex_apps_open_page with examples."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			record, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": tc.input}}}})
			filtered, err := (CodexAdapter{}).FilterJSONL(strings.NewReader(string(record)))
			if err != nil {
				t.Fatal(err)
			}
			retained := bytes.Join(filtered.Records, []byte("\n"))
			for _, boundary := range []string{" \n    ", " \t\r\n\t"} {
				if at := strings.Index(tc.input, boundary); at >= 0 {
					var kept map[string]any
					if err := json.Unmarshal(filtered.Records[0], &kept); err != nil {
						t.Fatal(err)
					}
					payload := kept["payload"].(map[string]any)
					text := payload["content"].([]any)[0].(map[string]any)["text"]
					if text != tc.input[at:] {
						t.Fatalf("retained text %q want %q", text, tc.input[at:])
					}
				}
			}
			refiltered, err := (CodexAdapter{}).FilterJSONL(bytes.NewReader(retained))
			if err != nil || !bytes.Equal(retained, bytes.Join(refiltered.Records, []byte("\n"))) {
				t.Fatalf("filter did not preserve its output: %v", err)
			}
			bundle := parserTestBundle(t, "codex", CodexAdapter{}, filtered)
			analysis, err := ParseCodex(context.Background(), bundle)
			if err != nil {
				t.Fatal(err)
			}
			wantTurns := 0
			if tc.want != "" {
				wantTurns = 1
			}
			if len(analysis.View.Turns) != wantTurns {
				t.Fatalf("turns %d want %d", len(analysis.View.Turns), wantTurns)
			}
			if analysis.Facts.TextTitle != tc.want {
				t.Fatalf("title %q want %q", analysis.Facts.TextTitle, tc.want)
			}
			facts, err := PreviewRecord("codex", record)
			if err != nil {
				t.Fatal(err)
			}
			if facts.Title != tc.want {
				t.Fatalf("preview %+v", facts)
			}
		})
	}
}

func TestClaudeNamingOnlyChangeAcrossCodecUpgradeRejectsOtherEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*archive.SourceBundle)
		want   bool
	}{
		{"codec provenance", func(b *archive.SourceBundle) {}, true},
		{"conversation", func(b *archive.SourceBundle) {
			b.NativeRecords[0]["message"].(map[string]any)["content"] = "New human request"
		}, false},
		{"owner", func(b *archive.SourceBundle) { b.NativeSessionID = "other" }, false},
		{"harness", func(b *archive.SourceBundle) { b.Capture.Harness.Version = "different" }, false},
		{"supplemental", func(b *archive.SourceBundle) {
			b.SupplementalEvidence = []archive.SupplementalEvidence{{Kind: archive.EvidenceKindLifecycleHook}}
		}, false},
		{"source format", func(b *archive.SourceBundle) { b.Capture.SourceFormat = "codex-jsonl" }, false},
		{"links", func(b *archive.SourceBundle) {
			b.LinkedSessions = []archive.LinkedSessionReference{{SessionID: "child"}}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before := claudeLabelLines(t)
			before.Capture.FilterVersion = "15"
			before.Capture.AdapterVersion = "0.15.0"
			after := claudeLabelLines(t, `{"type":"ai-title","aiTitle":"Generated","sessionId":"s"}`)
			tc.change(&after)
			if got := ClaudeNamingOnlyChange(before, after); got != tc.want {
				t.Fatalf("naming-only = %v, want %v", got, tc.want)
			}
		})
	}
}
