package nativesessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

type scopeCancelFS struct {
	OS
	cancel      context.CancelFunc
	scope       string
	resolutions int
	wasCanceled bool
}

func (f *scopeCancelFS) EvalSymlinks(path string) (string, error) {
	if path == f.scope {
		f.cancel()
		f.wasCanceled = true
	} else if f.wasCanceled {
		f.resolutions++
	}
	return f.OS.EvalSymlinks(path)
}

func TestDiscoveryCancellationDuringCheckoutScoping(t *testing.T) {
	root, cwd := nativeFixture(t, 3)
	for i := range 3 {
		dir := filepath.Join(cwd, strconv.Itoa(i))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("session-%05d", i)
		record, _ := json.Marshal(map[string]any{"type": "user", "sessionId": id, "cwd": dir})
		if err := os.WriteFile(filepath.Join(root.Path, "project", id+".jsonl"), append(record, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	files := &scopeCancelFS{cancel: cancel, scope: cwd}
	result, err := Discover(ctx, files, []StoreRoot{root}, Scope{Directories: []string{cwd}}, discoveryLimits())
	t.Logf("context=%v resultError=%v candidates=%d extraCanonicalResolutionsAfterCancel=%d", ctx.Err(), err, len(result.Candidates), files.resolutions)
	if !errors.Is(err, context.Canceled) || files.resolutions != 0 || len(result.Candidates) != 0 {
		t.Error("discovery ignores cancellation during checkout-scoping stage")
	}
}
