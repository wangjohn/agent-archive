package cli

import (
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/discovery"
)

// Installation and successful discovery must not imply hooks ran or need approval.
func TestStatusOnboardingKeepsOptionalHooksInDetails(t *testing.T) {
	t.Parallel()
	for _, observed := range []bool{false, true} {
		app := appStatus{Name: "codex", Hooks: "installed", Trust: "unknown", HookObserved: observed,
			Sessions: 4, PublishedSessions: 2, VerifiedSessions: 1,
			Discovery: &discovery.Health{Enabled: true, Supported: true, LastAttempt: renderNow},
		}
		view := statusView{configured: true, Background: "loaded", Apps: []appStatus{app}}
		basic := renderStatus(view, false)
		for _, unwanted := range []string{"hooks", "Hook", "registered →", "Last discovery attempt", "discovery:"} {
			if strings.Contains(basic, unwanted) {
				t.Fatalf("basic status exposes optional hook or discovery details %q:\n%s", unwanted, basic)
			}
		}
		for _, wanted := range []string{"automatic capture; included projects", "4 sessions · 2 archived (1 verified)"} {
			if !strings.Contains(basic, wanted) {
				t.Fatalf("missing %q:\n%s", wanted, basic)
			}
		}
		verbose := renderVerboseStatus(view)
		observation := "Hooks installed; execution not yet observed."
		if observed {
			observation = "Hooks installed; execution observed."
		}
		for _, wanted := range []string{observation, "Hook approval is optional", "Hook trust: unknown", "4 registered → 0 queued → 2 published → 1 read-back verified"} {
			if !strings.Contains(verbose, wanted) {
				t.Fatalf("verbose status missing %q:\n%s", wanted, verbose)
			}
		}
	}
}

func TestStatusOnboardingKeepsRequiredHookAndCaptureWarnings(t *testing.T) {
	t.Parallel()
	app := appStatus{Name: "codex", Hooks: "installed", Trust: "unknown"}
	view := statusView{configured: true, Background: "loaded", Apps: []appStatus{app}}
	if text := renderStatus(view, false); !strings.Contains(text, "Approve the archive hooks with /hooks") || strings.Contains(text, "hooks on") {
		t.Fatalf("required hook instructions missing or misleading:\n%s", text)
	}
	app.Discovery = &discovery.Health{Enabled: true, Supported: true, LastAttempt: renderNow, Outcomes: map[string]int{"invalid_identity": 2}}
	view.Apps[0] = app
	text := renderStatus(view, false)
	if !strings.Contains(text, "! Last scan skipped 2 observations") || strings.Contains(text, "invalid identity") {
		t.Fatalf("basic scan warning missing or too detailed:\n%s", text)
	}
	if verbose := renderVerboseStatus(view); !strings.Contains(verbose, "invalid identity: 2") {
		t.Fatalf("verbose scan reasons missing:\n%s", verbose)
	}
	app.Discovery.Outcomes = nil
	app.Hooks = hooksBroken
	view.Apps[0] = app
	if text := renderStatus(view, false); !strings.Contains(text, "Capture hooks need attention") {
		t.Fatalf("broken hooks hidden:\n%s", text)
	}
}
