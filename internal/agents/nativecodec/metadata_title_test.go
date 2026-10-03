package nativecodec

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuildMetadataDerivesTitleFromFirstHumanPrompt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	bundle := SourceBundle{
		SchemaVersion: SourceSchemaVersion, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p",
		Capture: SourceCapture{
			Harness: Harness{Name: "claude"}, AdapterName: "claude", AdapterVersion: "1",
			SourceFormat: "claude-jsonl", FilterVersion: FilterVersion, CapturedAt: now,
		},
		NativeRecords: []map[string]any{
			{"type": "user", "uuid": "u1", "timestamp": now.Format(time.RFC3339), "message": map[string]any{"role": "user", "content": "  Fix   flaky\nOAuth callback tests  "}},
			{"type": "assistant", "uuid": "a1", "timestamp": now.Format(time.RFC3339), "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "ok"}}}},
		},
	}
	ref := SourceReference{Key: "sessions/claude/a/source." + strings.Repeat("a", 64) + ".jsonl.gz", SHA256: strings.Repeat("a", 64)}
	m, err := BuildMetadata(bundle, "machine", now, now, ref, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "Fix flaky OAuth callback tests" {
		t.Fatalf("title=%q parser=%v", m.Title, m.Parser)
	}
	if m.Parser.Version != DefaultParserVersion {
		t.Fatalf("parser=%q", m.Parser.Version)
	}
}

func TestDeriveSessionTitleFromNativeText(t *testing.T) {
	t.Parallel()
	texts := []TextTranscript{{Format: "cursor-text", Content: "user: Tighten the intro.\nassistant: Done.\n"}}
	if got := deriveSessionTitle(NormalizedView{}, texts); got != "Tighten the intro." {
		t.Fatalf("got %q", got)
	}
}

// A Cursor prompt arrives wrapped in a <timestamp> line and <user_query>
// tags, which are not what the person typed and so are not the title, as
// they are not the prompt handoff shows.
func TestCursorTitleDropsTheQueryWrapper(t *testing.T) {
	t.Parallel()
	prompt := "<timestamp>Sunday, Sep 27, 2026, 10:31 PM (UTC-7)</timestamp>\n<user_query>\nhelp me figure out why the widget test flakes\n</user_query>"
	line, err := json.Marshal(map[string]any{"role": "user", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": prompt}}}})
	if err != nil {
		t.Fatal(err)
	}
	reply := `{"role":"assistant","message":{"content":[{"type":"text","text":"Looking."}]}}`
	filtered, err := CursorAdapter{}.FilterJSONL(strings.NewReader(string(line) + "\n" + reply + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	bundle := parserTestBundle(t, "cursor", CursorAdapter{}, filtered)
	const want = "help me figure out why the widget test flakes"
	if got := labelsOf(t, bundle).Title; got != want {
		t.Fatalf("SessionLabels title = %q, want %q", got, want)
	}
	if got := parserTestMetadata(t, bundle).Title; got != want {
		t.Fatalf("metadata title = %q, want %q", got, want)
	}
}

func TestDeriveSessionTitleFromWrappedNativeText(t *testing.T) {
	t.Parallel()
	texts := []TextTranscript{{Format: "cursor-text", Content: "user: <timestamp>Sunday, Sep 27, 2026, 10:31 PM (UTC-7)</timestamp>\n<user_query>\nTighten the intro.\n</user_query>\nassistant: Done.\n"}}
	if got := deriveSessionTitle(NormalizedView{}, texts); got != "Tighten the intro." {
		t.Fatalf("got %q", got)
	}
}

// The wrapper is Cursor's: another harness's prompt that starts with the same
// tags keeps them in its title.
func TestNonCursorTitleKeepsTagsAsWritten(t *testing.T) {
	t.Parallel()
	const prompt = "<timestamp>noon</timestamp> <user_query>an XML example</user_query>"
	bundle := claudeLines(t, `{"type":"user","uuid":"u1","sessionId":"s","timestamp":"2026-09-30T10:00:00Z","message":{"role":"user","content":"`+prompt+`"}}`)
	if got := labelsOf(t, bundle).Title; got != prompt {
		t.Fatalf("title = %q, want %q", got, prompt)
	}
	view := NormalizedView{Turns: []NormalizedTurn{{Kind: TurnKindHumanPrompt, Text: prompt}}}
	if got := deriveSessionTitle(view, nil); got != prompt {
		t.Fatalf("deriveSessionTitle = %q, want %q", got, prompt)
	}
}
