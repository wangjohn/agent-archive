package nativecodec

import (
	"testing"
)

func TestNativeToolSummaryPreservesGenericFallback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		input      map[string]any
		root, want string
	}{
		{"Grep", map[string]any{"path": "/p/src"}, "/p", `{"path":"/p/src"}`},
		{"Glob", map[string]any{"path": "/p/src", "title": "search workspace"}, "/p", "search workspace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolSummary(tc.name, tc.input, nil, tc.root); got != tc.want {
				t.Fatalf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}
