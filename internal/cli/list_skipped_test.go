package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// A sidecar that does not validate (written by a newer version, or damaged)
// is left out of `list` with a warning on stderr; the rest is listed and
// stdout keeps its format.
func TestListWarnsAboutInvalidSidecarAndListsTheRest(t *testing.T) {
	env, mem, id := publishedFixture(t)
	bad := "sessions/codex/newer-session/metadata.json"
	if err := mem.Put(context.Background(), bad, []byte(`{"schema_version":99}`)); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"list", "--no-cache"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), id) || !strings.Contains(out.String(), "1 session(s).") {
		t.Fatalf("stdout:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "warning: skipped a session whose metadata could not be read") || !strings.Contains(errOut.String(), bad) {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}
