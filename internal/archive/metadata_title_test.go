package archive

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDeriveSessionTitleSkipsEmptyPrompts(t *testing.T) {
	t.Parallel()
	view := NormalizedView{Turns: []NormalizedTurn{
		{Kind: TurnKindHumanPrompt, Text: "   "},
		{Kind: TurnKindAssistant, Text: "hello"},
		{Kind: TurnKindHumanPrompt, Text: "Real question"},
	}}
	if got := func() string { l, _ := LabelsFromAnalysis(Analysis{View: view}); return l.Title }(); got != "Real question" {
		t.Fatalf("got %q", got)
	}
}

func TestCollapseSessionTitleTruncates(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("word ", 40)
	got := collapseSessionTitle(long)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis: %q", got)
	}
	if len([]rune(got)) != sessionTitleLimit+1 {
		t.Fatalf("len=%d got=%q", len([]rune(got)), got)
	}
}

func TestCollapseSessionTitleKeepsTextBeyondOldLimit(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("a", 72) + " distinguish this session"
	if got := collapseSessionTitle(text); got != text {
		t.Fatalf("title lost distinguishing text: %q", got)
	}
}

func TestApplyProjectName(t *testing.T) {
	t.Parallel()
	var m Metadata
	m.ApplyProjectName(filepath.Join(string(filepath.Separator), "Users", "alex", "src", "agent-archive"))
	if m.ProjectName != "agent-archive" {
		t.Fatalf("project_name=%q", m.ProjectName)
	}
}
