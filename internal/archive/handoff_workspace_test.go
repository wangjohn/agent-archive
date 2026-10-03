package archive

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func TestCompareWorkspace(t *testing.T) {
	t.Parallel()
	recorded := HandoffWorkspace{Directory: "widgets", Branch: "fix/widget-test"}
	for name, tc := range map[string]struct {
		root     string
		checkout HandoffCheckout
		want     HandoffWorkspace
	}{
		"no checkout given claims nothing": {root: "/Users/someone/widgets", want: recorded},
		"no recorded directory": {checkout: HandoffCheckout{Directories: []string{"/home/me/widgets"}, Branch: "fix/widget-test"},
			want: recorded},
		"the same directory": {root: "/Users/someone/widgets", checkout: HandoffCheckout{Directories: []string{"/Users/someone/widgets"}, Branch: "fix/widget-test"},
			want: recorded},
		"a subdirectory of the recorded one": {root: "/Users/someone/widgets", checkout: HandoffCheckout{Directories: []string{"/Users/someone/widgets/pkg/x"}},
			want: recorded},
		"a session started in a subdirectory": {root: "/Users/someone/widgets/pkg", checkout: HandoffCheckout{Directories: []string{"/Users/someone/widgets"}},
			want: recorded},
		"either spelling of the checkout": {root: "/private/var/widgets", checkout: HandoffCheckout{Directories: []string{"/var/widgets", "/private/var/widgets"}},
			want: recorded},
		"a sibling with a shared prefix": {root: "/Users/someone/widgets", checkout: HandoffCheckout{Directories: []string{"/Users/someone/widgets-2"}},
			want: HandoffWorkspace{Directory: "widgets", Branch: "fix/widget-test", Elsewhere: true}},
		"another machine's path": {root: "/Users/someone/widgets", checkout: HandoffCheckout{Directories: []string{"/home/me/src/widgets"}, Branch: "main"},
			want: HandoffWorkspace{Directory: "widgets", Branch: "fix/widget-test", Elsewhere: true, CurrentBranch: "main"}},
		"another branch, same place": {root: "/a/widgets", checkout: HandoffCheckout{Directories: []string{"/a/widgets"}, Branch: "main"},
			want: HandoffWorkspace{Directory: "widgets", Branch: "fix/widget-test", CurrentBranch: "main"}},
		"a detached checkout has no branch to differ": {root: "/a/widgets", checkout: HandoffCheckout{Directories: []string{"/a/widgets"}},
			want: recorded},
	} {
		if got := compareWorkspace(recorded, tc.root, tc.checkout); got != tc.want {
			t.Errorf("%s: %+v, want %+v", name, got, tc.want)
		}
	}
	// A session that recorded no branch has nothing to compare.
	if got := compareWorkspace(HandoffWorkspace{Directory: "w"}, "/a/w", HandoffCheckout{Directories: []string{"/a/w"}, Branch: "main"}); got.CurrentBranch != "" {
		t.Errorf("branch difference claimed without a recorded branch: %+v", got)
	}
}

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
	golden.Check(t, filepath.Join("testdata", "handoff", "claude-elsewhere.md"), got)
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
