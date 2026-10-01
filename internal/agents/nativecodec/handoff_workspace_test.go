package nativecodec

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// The handoff for a checkout elsewhere says so, in the workspace section, and
// only there; a handoff built without a checkout is unchanged (the harness
// goldens).
func TestHandoffNotesADifferentCheckout(t *testing.T) {
	t.Parallel()
	bundle := handoffBundle(t, "claude")
	elsewhere := HandoffCheckout{Directories: []string{"/home/me/src/widgets"}, Branch: "main"}
	h, err := BuildHandoff(bundle, nil, HandoffOptions{Source: "archive", Checkout: elsewhere})
	if err != nil {
		t.Fatal(err)
	}
	if !h.Workspace.Elsewhere || h.Workspace.CurrentBranch != "main" || h.Workspace.Directory != "widgets" {
		t.Fatalf("workspace = %+v", h.Workspace)
	}
	got := RenderHandoffMarkdown(h, HandoffRenderOptions{Preamble: true})
	golden.Check(t, filepath.Join("../../archive/testdata", "handoff", "claude-elsewhere.md"), got)
	// The full recorded path never reaches the document.
	if strings.Contains(string(got), "/Users/someone") || strings.Contains(string(got), "/home/me") {
		t.Errorf("a full directory path is in the handoff:\n%s", got)
	}
	// The same checkout adds nothing.
	same, err := BuildHandoff(bundle, nil, HandoffOptions{Source: "local", Checkout: HandoffCheckout{Directories: []string{"/Users/someone/widgets"}, Branch: "fix/widget-test"}})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := BuildHandoff(bundle, nil, HandoffOptions{Source: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if string(RenderHandoffMarkdown(same, HandoffRenderOptions{Preamble: true})) != string(RenderHandoffMarkdown(plain, HandoffRenderOptions{Preamble: true})) {
		t.Error("a handoff for the recorded checkout differs from one built without a checkout")
	}
}

// Text taken from the current checkout (a branch name a repository chose) is
// display text like everything else in the document.
func TestHandoffCurrentBranchIsDisplayText(t *testing.T) {
	t.Parallel()
	h, err := BuildHandoff(handoffBundle(t, "claude"), nil, HandoffOptions{Checkout: HandoffCheckout{Directories: []string{"/x"}, Branch: "main\x1b]52;c;eA==\x07\n## Injected"}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(RenderHandoffMarkdown(h, HandoffRenderOptions{Preamble: true}))
	if strings.ContainsAny(got, "\x1b\x07") || strings.Contains(got, "\n## Injected") {
		t.Errorf("branch escaped its line:\n%s", got)
	}
}
