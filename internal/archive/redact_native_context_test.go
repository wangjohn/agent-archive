package archive

import "testing"

func TestOpenPageContextStrippingPreservesHumanText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"<external_codex_apps_open_page>Page data</external_codex_apps_open_page>", ""},
		{"<external_codex_apps_open_page>Page data</external_codex_apps_open_page>Summarize this page.", "Summarize this page."},
		{"Keep this <external_codex_apps_open_page>outer<external_codex_apps_open_page>inner</external_codex_apps_open_page>tail</external_codex_apps_open_page> request", "Keep this <external_codex_apps_open_page>outer<external_codex_apps_open_page>inner</external_codex_apps_open_page>tail</external_codex_apps_open_page> request"},
		{"<external_codex_apps_open_page>unfinished", ""},
		{"Why does `<external_codex_apps_open_page>...</external_codex_apps_open_page>` appear?", "Why does `<external_codex_apps_open_page>...</external_codex_apps_open_page>` appear?"},
		{"```xml\n<external_codex_apps_open_page>example</external_codex_apps_open_page>\n```", "```xml\n<external_codex_apps_open_page>example</external_codex_apps_open_page>\n```"},
		{"<external_codex_apps_open_page-example>Keep this</external_codex_apps_open_page-example>", "<external_codex_apps_open_page-example>Keep this</external_codex_apps_open_page-example>"},
		{"    <external_codex_apps_open_page>example</external_codex_apps_open_page>", "    <external_codex_apps_open_page>example</external_codex_apps_open_page>"},
		{"<external_codex_apps_open_page>Context</external_codex_apps_open_page>\n    <external_codex_apps_open_page>example</external_codex_apps_open_page>", "\n    <external_codex_apps_open_page>example</external_codex_apps_open_page>"},
		{"\t<external_codex_apps_open_page>example</external_codex_apps_open_page>", "\t<external_codex_apps_open_page>example</external_codex_apps_open_page>"},
		{"    <external_codex_apps_open_page>example</external_codex_apps_open_page>\n<system-reminder>Injected</system-reminder>", "    <external_codex_apps_open_page>example</external_codex_apps_open_page>"},
		{"Explain external_codex_apps_open_page in an example.", "Explain external_codex_apps_open_page in an example."},
	} {
		_, got := stripInjectedInstructions(tc.input)
		if got != tc.want {
			t.Fatalf("got %q want %q", got, tc.want)
		}
	}
}
