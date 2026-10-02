package archive

import (
	"testing"
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
