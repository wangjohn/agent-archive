package archive

import (
	"testing"
)

// workspaceFile shows and deduplicates a named file relative to the
// workspace root, however the call spelled it.
func TestWorkspaceFileNormalizesSpellings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		file string
		root string
		want string
	}{
		{"/work/widget/a.go", "/work/widget", "a.go"},
		{"a.go", "/work/widget", "a.go"},
		{"./a.go", "/work/widget", "a.go"},
		{"../widget/d.go", "/work/widget", "d.go"},
		{"sub/../b.go", "/work/widget", "b.go"},
		{"/work/other/e.go", "/work/widget", "/work/other/e.go"},
		{"../other/e.go", "/work/widget", "../other/e.go"},
		{"sub/../../other/e.go", "/work/widget", "../other/e.go"},
		{"../widget", "/work/widget", ""},
		{"/a.go", "/", "a.go"},
		{"a.go", "/", "a.go"},
		{"/work/widgetry/f.go", "/work/widget", "/work/widgetry/f.go"},
		{".", "/work/widget", ""},
		{"/work/widget", "/work/widget", ""},
		{"", "/work/widget", ""},
		{"./a.go", "", "a.go"},
		{".", "", ""},
		{"/x//y.go", "", "/x/y.go"},
		{`C:\work\widget\a.go`, `C:\work\widget`, "a.go"},
		{`c:\work\widget\sub\b.go`, `C:\work\widget`, "sub/b.go"},
		{`sub\b.go`, `C:\work\widget`, "sub/b.go"},
		{`D:\elsewhere\c.go`, `C:\work\widget`, "D:/elsewhere/c.go"},
		{`C:\work\widget\a.go`, "", "C:/work/widget/a.go"},
		{`odd\name.go`, "/work/widget", `odd\name.go`},
	} {
		if got := workspaceFile(tc.file, tc.root); got != tc.want {
			t.Errorf("workspaceFile(%q, %q) = %q, want %q", tc.file, tc.root, got, tc.want)
		}
	}
}
