package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

var summaryNow = time.Date(2026, 9, 29, 13, 2, 0, 0, time.UTC)

func intPtr(n int) *int { return &n }

// summaryFixture is a Claude Code session with every row the summary shows.
func summaryFixture() sessionView {
	importedAt := time.Date(2026, 9, 29, 12, 30, 0, 0, time.UTC)
	return sessionView{
		Metadata: archive.Metadata{
			SessionID:     "03e60c25f1a04b7c9d2e8f6a1b3c5d7e",
			Title:         "Fix flaky OAuth callback tests",
			ProjectName:   "agent-archive",
			StartedAt:     time.Date(2026, 9, 29, 10, 14, 0, 0, time.UTC),
			CapturedAt:    time.Date(2026, 9, 29, 11, 2, 0, 0, time.UTC),
			Harness:       archive.Harness{Name: "claude", Version: "2.4.1"},
			Parser:        archive.ParserInfo{Name: "claude", Version: "0.12.0", Status: archive.ParserStatusPartial},
			FilterVersion: "12",
			State:         archive.MetadataStateIdle,
			TurnOutcome:   archive.TurnOutcomeCompleted,
			Models: []archive.ModelSummary{
				{Attributes: map[string]string{"gen_ai.request.model": "claude-haiku-4-5"}, TurnCount: intPtr(4)},
				{Attributes: map[string]string{"gen_ai.request.model": "claude-opus-5-5", "agent_archive.request.reasoning_level": "high"}, TurnCount: intPtr(31)},
				{Attributes: map[string]string{"gen_ai.request.model": "claude-opus-5-5", "agent_archive.request.reasoning_level": "high"}, Source: archive.ModelSummarySourceHook},
				{Attributes: map[string]string{"gen_ai.request.model": "<synthetic>"}, TurnCount: intPtr(1)},
			},
			SkillsUsed: []archive.SkillUse{{Name: "simplify"}, {Name: "code-review"}},
			Counts: archive.Counts{
				Turns: intPtr(35), Messages: intPtr(212), ToolCalls: intPtr(148),
				UserShellCommands: intPtr(3), Compactions: intPtr(1),
			},
			CaptureGaps: []archive.CaptureGap{
				{Code: archive.CaptureGapImportedWithoutHookEvidence, Detail: "No hook observed this session before it was imported (imported_at): activity before then has no hook lifecycle events, final-response text, or skill inventory."},
				{Code: "tool_result_truncated", Record: 12},
				{Code: "tool_result_truncated", Record: 40},
			},
			LinkedSessions: []archive.LinkedSessionReference{{SessionID: "child-1"}, {SessionID: "child-2"}},
			Origin:         archive.SessionOriginImport,
			ImportedAt:     &importedAt,
		},
		LinkedAvailability: []reader.LinkedAvailability{
			{SessionID: "child-1", State: reader.LinkedStateMetadataAvailable},
			{SessionID: "child-2", State: reader.LinkedStateUnavailableOrExpired},
		},
	}
}

func renderSummaryText(view sessionView, opts summaryOptions) string {
	var b bytes.Buffer
	renderSessionSummary(&b, view, opts)
	return b.String()
}

// Regenerate with `go test ./internal/cli -run TestSessionSummaryGolden
// -update` and review the diff.
func TestSessionSummaryGolden(t *testing.T) {
	t.Parallel()
	opts := summaryOptions{Now: summaryNow, Location: time.UTC, Hints: true}
	golden.Check(t, filepath.Join("testdata", "show", "summary.txt"), []byte(renderSummaryText(summaryFixture(), opts)))
	opts.Style = textStyle{color: true}
	golden.Check(t, filepath.Join("testdata", "show", "summary-color.txt"), []byte(renderSummaryText(summaryFixture(), opts)))
}

// Unknown counts are left out, not shown as zero, and a session with no
// gaps, links, models, or skills has no rows for them.
func TestSessionSummaryOmitsAbsentData(t *testing.T) {
	t.Parallel()
	view := sessionView{Metadata: archive.Metadata{
		SessionID: "abcdef0123456789abcdef0123456789",
		Harness:   archive.Harness{Name: "codex"},
		Counts:    archive.Counts{Turns: intPtr(2), ToolCalls: intPtr(0)},
	}}
	text := renderSummaryText(view, summaryOptions{Now: summaryNow, Location: time.UTC})
	if !strings.HasPrefix(text, "abcdef01\n") {
		t.Fatalf("title does not fall back to the short ID:\n%s", text)
	}
	if !strings.Contains(text, "Activity  2 turns · 0 tool calls\n") {
		t.Fatalf("activity row:\n%s", text)
	}
	for _, absent := range []string{"message", "compaction", "When", "Model", "Skills", "Subagents", "Parent", "Imported", "Capture gaps", "Transcript:", "completed"} {
		if strings.Contains(text, absent) {
			t.Fatalf("%q shown for absent data:\n%s", absent, text)
		}
	}
	if !strings.Contains(text, "Agent     Codex\n") || !strings.Contains(text, "origin hook") {
		t.Fatalf("agent or provenance missing:\n%s", text)
	}
}

func TestSessionSummaryParentAndSpanAcrossDays(t *testing.T) {
	t.Parallel()
	view := sessionView{Metadata: archive.Metadata{
		SessionID:       "abcdef0123456789abcdef0123456789",
		ParentSessionID: "0123456789abcdef0123456789abcdef",
		Harness:         archive.Harness{Name: "claude"},
		StartedAt:       time.Date(2025, 12, 31, 23, 0, 0, 0, time.UTC),
		CapturedAt:      time.Date(2026, 1, 1, 1, 30, 0, 0, time.UTC),
		State:           archive.MetadataStateActive,
	}}
	text := renderSummaryText(view, summaryOptions{Now: summaryNow, Location: time.UTC})
	for _, want := range []string{"Parent    01234567\n", "When      Dec 31 2025, 23:00 → Jan 1, 01:30 (2h 30m span)\n", "! active\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
}

// Every metadata string is one display line: escape sequences, control
// characters, and bidi overrides from the bucket never reach the terminal.
func TestSessionSummaryNeutralizesEscapes(t *testing.T) {
	t.Parallel()
	view := summaryFixture()
	view.Title = "evil\x1b[2J\x1b]52;c;cGFzdGU=\x07title\u202e\nsecond line"
	view.ProjectName = "proj\x1b[31m"
	view.Harness.Version = "1.0\u009b2J"
	view.SkillsUsed = []archive.SkillUse{{Name: "skill\x07"}}
	view.CaptureGaps = []archive.CaptureGap{{Code: "gap\x1b[1m", Detail: "detail\r\nmore"}}
	view.Models = []archive.ModelSummary{{Attributes: map[string]string{"gen_ai.request.model": "model\x1b[0m", "agent_archive.request.reasoning_level": "hi\x9b"}}}
	text := renderSummaryText(view, summaryOptions{Now: summaryNow, Location: time.UTC, Hints: true})
	for _, r := range text {
		if (r < 0x20 && r != '\n') || r == 0x7f || (r >= 0x80 && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) {
			t.Fatalf("control %U in summary:\n%q", r, text)
		}
	}
	if !strings.Contains(text, "evil[2J]52;c;cGFzdGU=title second line") {
		t.Fatalf("title not one display line:\n%s", text)
	}
}

func TestSessionSummaryModelLines(t *testing.T) {
	t.Parallel()
	got := summaryModels([]archive.ModelSummary{
		{Attributes: map[string]string{"gen_ai.request.model": "gpt-5", "gen_ai.response.model": "gpt-5-2026-08"}, TurnCount: intPtr(1)},
		{Attributes: map[string]string{"agent_archive.request.model_label": "Auto"}},
		{Attributes: map[string]string{"gen_ai.request.model": "o4", "agent_archive.request.reasoning_level": "low"}, TurnCount: intPtr(3)},
		{Attributes: map[string]string{"gen_ai.request.model": "o4", "agent_archive.request.reasoning_level": "low"}, TurnCount: intPtr(2)},
		{Attributes: map[string]string{}},
	})
	want := []string{"o4 (low reasoning) · 5 responses", "gpt-5-2026-08 · 1 response", "Auto"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("models = %q, want %q", got, want)
	}
}

func TestSessionSummaryTruncatesLongLines(t *testing.T) {
	t.Parallel()
	view := summaryFixture()
	view.Title = strings.Repeat("long title ", 20)
	view.SkillsUsed = nil
	for i := range 30 {
		view.SkillsUsed = append(view.SkillsUsed, archive.SkillUse{Name: "skill-" + strings.Repeat("x", i)})
	}
	text := renderSummaryText(view, summaryOptions{Now: summaryNow, Location: time.UTC, Style: textStyle{width: 40}, Hints: true})
	for line := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		if w := visibleWidth(line); w > 40 {
			t.Fatalf("line of %d columns: %q", w, line)
		}
	}
	if !strings.Contains(text, "…") {
		t.Fatalf("no ellipsis:\n%s", text)
	}
}

// transcriptFixture filters the Claude handoff fixture into a bundle, the
// same path a capture takes.
func transcriptFixture(t *testing.T) archive.Transcript {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "archive", "testdata", "handoff", "claude.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := archive.NewAdapter("claude")
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := adapter.FilterJSONL(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{
		ArchiveSessionID: summaryFixture().SessionID, NativeSessionID: "native-claude", ProjectID: "project", ProjectRoot: "/Users/someone/widgets",
		Harness: archive.Harness{Name: adapter.Name()}, SessionStartedAt: time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC),
	}
	bundle, err := archive.NewSourceBundle(reg, adapter, filtered, time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := buildTranscript(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return transcript
}

// Regenerate with `go test ./internal/cli -run TestTranscriptGolden -update`
// and review the diff.
func TestTranscriptGolden(t *testing.T) {
	t.Parallel()
	transcript := transcriptFixture(t)
	for name, full := range map[string]bool{"transcript.txt": false, "transcript-full.txt": true} {
		var b bytes.Buffer
		renderTranscript(&b, summaryFixture(), transcript, transcriptOptions{summaryOptions: summaryOptions{Now: summaryNow, Location: time.UTC}, Full: full})
		golden.Check(t, filepath.Join("testdata", "show", name), b.Bytes())
	}
}

// show --transcript pages the readable transcript; --full adds results;
// --transcript --json is the former --normalized output.
func TestShowTranscriptFlags(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	run := func(args ...string) (string, string, int) {
		var out, errOut bytes.Buffer
		code := Run(args, nil, &out, &errOut, env)
		return out.String(), errOut.String(), code
	}
	out, errOut, code := run("show", id, "--transcript")
	if code != 0 || !strings.Contains(out, "visible") || strings.Contains(out, `"turns"`) {
		t.Fatalf("--transcript: code=%d stderr=%s\n%s", code, errOut, out)
	}
	if _, errOut, code := run("show", id, "--full"); code != 2 || !strings.Contains(errOut, "--full needs --transcript") {
		t.Fatalf("--full alone: code=%d stderr=%s", code, errOut)
	}
	if out, errOut, code := run("show", id, "--transcript", "--full"); code != 0 || !strings.Contains(out, "visible") {
		t.Fatalf("--transcript --full: code=%d stderr=%s\n%s", code, errOut, out)
	}
	transcriptJSON, errOut, code := run("show", id, "--transcript", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("--transcript --json: code=%d stderr=%s", code, errOut)
	}
	normalized, errOut, code := run("show", id, "--normalized")
	if code != 0 || normalized != transcriptJSON {
		t.Fatalf("--normalized differs from --transcript --json: code=%d\n%s\n---\n%s", code, normalized, transcriptJSON)
	}
	if strings.TrimSpace(errOut) != "agent-archive: show: --normalized is deprecated; use --transcript --json" {
		t.Fatalf("deprecation note = %q", errOut)
	}
}

// On a terminal, show --transcript goes through the pager unless
// --no-pager.
func TestShowTranscriptPages(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	var out, errOut bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == any(&out) }
	var paged []string
	env.RunPager = func(_ context.Context, command string, _ []string, in io.Reader, _, _ io.Writer) error {
		text, err := io.ReadAll(in)
		paged = append(paged, command+"\n"+string(text))
		return err
	}
	if code := Run([]string{"show", id, "--transcript"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if len(paged) != 1 || !strings.HasPrefix(paged[0], "less -FRX --mouse --wheel-lines=3 ") || !strings.Contains(paged[0], "visible") || out.Len() != 0 {
		t.Fatalf("paged=%q out=%q", paged, out.String())
	}
	if code := Run([]string{"show", id, "--transcript", "--no-pager"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if len(paged) != 1 || !strings.Contains(out.String(), "visible") {
		t.Fatalf("--no-pager paged: paged=%d out=%q", len(paged), out.String())
	}
}

// Transcript content needs an explicit request: bare show with
// --transcript is a usage error, since the browser's t key asks for it.
func TestShowTranscriptNeedsSessionID(t *testing.T) {
	t.Parallel()
	env, _, _ := publishedFixture(t)
	stdin := strings.NewReader("1\n")
	var out, errOut bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(&out) }
	if code := Run([]string{"show", "--transcript"}, stdin, &out, &errOut, env); code != 2 || out.Len() != 0 {
		t.Fatalf("code=%d out=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "--transcript needs a SESSION_ID") {
		t.Fatalf("stderr=%s", errOut.String())
	}
}

// Shell and local commands are shown where they ran; a shell command's
// output only with --full, a local command's always. A notice from the app
// heads its own exchange.
func TestTranscriptRendersCommandsAndNotices(t *testing.T) {
	t.Parallel()
	transcript := archive.Transcript{Exchanges: []archive.TranscriptExchange{
		{Kind: archive.TranscriptExchangeLeading, Steps: []archive.TranscriptStep{
			{Kind: archive.TranscriptStepShell, Text: "git status\ngit log", Output: "On branch main"},
			{Kind: archive.TranscriptStepCommand, Text: "/model", Output: "Set model to claude-opus-5"},
		}},
		{Kind: archive.TranscriptExchangeNotification, Text: "Background task completed", Steps: []archive.TranscriptStep{
			{Kind: archive.TranscriptStepText, Text: "The reviewer finished."},
		}},
	}}
	render := func(full bool) string {
		var b bytes.Buffer
		renderTranscript(&b, summaryFixture(), transcript, transcriptOptions{summaryOptions: summaryOptions{Now: summaryNow, Location: time.UTC}, Full: full})
		return b.String()
	}
	plain, full := render(false), render(true)
	for _, want := range []string{"  $ git status …\n", "  » /model\n      │ Set model to claude-opus-5\n", "── Background task completed ─", "The reviewer finished."} {
		if !strings.Contains(plain, want) {
			t.Fatalf("missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "On branch main") || !strings.Contains(full, "  $ git status …\n      │ On branch main\n") {
		t.Fatalf("shell output shown without --full, or not with it:\n%s\n---\n%s", plain, full)
	}
}

// A session ID from the bucket is display text too, even as a fallback
// title or in a hint.
func TestSessionSummaryNeutralizesSessionID(t *testing.T) {
	t.Parallel()
	view := sessionView{Metadata: archive.Metadata{SessionID: "\x1b[2J\u202eé0123456789", Harness: archive.Harness{Name: "codex"}}}
	for _, text := range []string{
		renderSummaryText(view, summaryOptions{Now: summaryNow, Hints: true}),
		func() string {
			var b bytes.Buffer
			renderTranscript(&b, view, archive.Transcript{}, transcriptOptions{})
			return b.String()
		}(),
	} {
		for _, r := range text {
			if (r < 0x20 && r != '\n') || (r >= 0x80 && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) {
				t.Fatalf("control %U in output:\n%q", r, text)
			}
		}
	}
	if got := shortSessionID("abcdefgé12"); got != "abcdefg" {
		t.Fatalf("short ID cut a character: %q", got)
	}
}

// The hints name the full ID, and the harness when show was given one.
func TestShowHintsNameAnUnambiguousID(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"show", id[:minShortSessionID], "--harness", "codex"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "agent-archive show "+id+" --harness codex --transcript") {
		t.Fatalf("hint:\n%s", out.String())
	}
}

func TestShowFullNeedsReadableTranscript(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	for _, args := range [][]string{{"show", id, "--transcript", "--full", "--json"}, {"show", id, "--normalized", "--full"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), "--full is for the readable transcript") {
			t.Fatalf("%v: code=%d out=%s stderr=%s", args, code, out.String(), errOut.String())
		}
	}
}
