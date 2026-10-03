package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// evalLines runs eval export and decodes each line of its output.
func evalLines(t *testing.T, env Env, args ...string) ([]map[string]any, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(append([]string{"eval", "export"}, args...), nil, &out, &errOut, env)
	var records []map[string]any
	for line := range strings.SplitSeq(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line %q is not JSON: %v", line, err)
		}
		records = append(records, record)
	}
	return records, errOut.String(), code
}

// A session captured by the hooks and published by sync exports at both
// details, one line each, from what sync uploaded.
func TestEvalExportPrintsOneRecordPerSession(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	full, errOut, code := evalLines(t, env, id)
	if code != 0 || errOut != "" || len(full) != 1 {
		t.Fatalf("exit %d, stderr %q, records %v", code, errOut, full)
	}
	record := full[0]
	if record["record"] != "session" || record["source"] != "archive" || record["detail"] != "full" || record["session_id"] != id || record["schema_version"] != float64(archive.EvalExportSchemaVersion) {
		t.Errorf("record = %v", record)
	}
	// This fixture's transcript has no human prompt: the list is present and
	// empty, never absent, at full detail.
	if prompts, ok := record["prompts"].([]any); !ok || len(prompts) != 0 || record["final_response"] == nil {
		t.Errorf("a full record without prompts or its final response: %v", record)
	}
	metadata, _, code := evalLines(t, env, "--detail", "metadata", id)
	if code != 0 || len(metadata) != 1 || metadata[0]["detail"] != "metadata" {
		t.Fatalf("exit %d, records %v", code, metadata)
	}
	for _, key := range []string{"prompts", "final_response", "files_edited", "feedback"} {
		if _, found := metadata[0][key]; found {
			t.Errorf("a metadata record has %s", key)
		}
	}
}

// The metadata pass reads only the sidecar: it still works when the source
// bundle is gone, while the full pass reports that session as an error
// record, and the export goes on to the next.
func TestEvalExportMetadataDetailReadsOnlyTheSidecar(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	keys, err := mem.List(context.Background(), "sessions/")
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range keys {
		if strings.Contains(object.Key, "/source.") {
			if err := mem.Delete(context.Background(), object.Key); err != nil {
				t.Fatal(err)
			}
		}
	}
	if records, errOut, code := evalLines(t, env, "--detail", "metadata", id); code != 0 || len(records) != 1 || records[0]["record"] != "session" {
		t.Fatalf("metadata without a source: exit %d, stderr %q, %v", code, errOut, records)
	}
	records, _, code := evalLines(t, env, id, id)
	if code != 1 || len(records) != 2 {
		t.Fatalf("full without a source: exit %d, %v", code, records)
	}
	for _, record := range records {
		if record["record"] != "error" || record["session_id"] != id || record["error"].(map[string]any)["code"] != "read_failed" {
			t.Errorf("record = %v", record)
		}
	}
}

// A sidecar that decodes but breaks the schema (here a negative count, as a
// hand-edited one might carry) is an error record at both details, never a
// session line that breaks the published contract.
func TestEvalExportRefusesAnInvalidSidecar(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	key, err := archive.MetadataObjectKey("codex", id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := mem.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var sidecar map[string]any
	if err := json.Unmarshal(raw, &sidecar); err != nil {
		t.Fatal(err)
	}
	sidecar["counts"].(map[string]any)["turns"] = -1
	if raw, err = json.Marshal(sidecar); err != nil {
		t.Fatal(err)
	}
	if err := mem.Put(context.Background(), key, raw); err != nil {
		t.Fatal(err)
	}
	for _, detail := range []string{"metadata", "full"} {
		records, _, code := evalLines(t, env, "--detail", detail, id)
		if code != 1 || len(records) != 1 || records[0]["record"] != "error" || records[0]["error"].(map[string]any)["code"] != "read_failed" {
			t.Errorf("--detail %s: exit %d, %v", detail, code, records)
		}
	}
}

// Each input gets its own line, in the order given; one that is not found is
// an error record and does not stop the rest.
func TestEvalExportReportsEachMissingSessionAndGoesOn(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	missing := strings.Repeat("0", 32)
	records, errOut, code := evalLines(t, env, missing, id, "fix the login bug")
	if code != 1 || errOut != "" || len(records) != 3 {
		t.Fatalf("exit %d, stderr %q, records %v", code, errOut, records)
	}
	wantCodes := []string{"not_found", "", "not_found"}
	for i, record := range records {
		if wantCodes[i] == "" {
			if record["record"] != "session" {
				t.Errorf("record %d = %v", i, record)
			}
			continue
		}
		if record["record"] != "error" || record["error"].(map[string]any)["code"] != wantCodes[i] {
			t.Errorf("record %d = %v", i, record)
		}
	}
	if records[2]["input"] != "fix the login bug" || records[2]["session_id"] != nil {
		t.Errorf("a title is not a session ID: %v", records[2])
	}
}

// --max-bytes bounds each record; the cut is recorded.
func TestEvalExportBoundsEachRecord(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	var out bytes.Buffer
	if code := Run([]string{"eval", "export", "--max-bytes", "400", id}, nil, &out, &bytes.Buffer{}, env); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var record archive.EvalExport
	if err := json.Unmarshal(out.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.Trimmed == nil || record.Trimmed.MaxBytes != 400 {
		t.Fatalf("trimmed = %+v in %d bytes", record.Trimmed, out.Len())
	}
	// Its metadata alone is larger than 400 bytes, which is never cut.
	if !record.Trimmed.ExceedsMaxBytes {
		t.Errorf("record is %d bytes, over its bound without saying so", out.Len())
	}
}

func TestEvalExportUsageErrors(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "name at least one SESSION_ID"},
		{[]string{"--detail", "summary", id}, "--detail must be metadata or full"},
		{[]string{"--max-bytes", "-1", id}, "--max-bytes must be 0 or more"},
		{[]string{"--harness", "vim", id}, "--harness"},
	} {
		records, errOut, code := evalLines(t, env, tc.args...)
		if code != 2 || len(records) != 0 || !strings.Contains(errOut, tc.want) {
			t.Errorf("%v: exit %d, stderr %q, records %v", tc.args, code, errOut, records)
		}
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"eval"}, nil, &out, &errOut, env); code != 2 || !strings.Contains(errOut.String(), "choose export") {
		t.Errorf("eval alone: exit %d, %q", code, errOut.String())
	}
}

// Before setup there is no archive to export from: stderr, exit 1, and no
// output a script could take for a record.
func TestEvalExportReportsNotSetUp(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), time.Now())
	records, errOut, code := evalLines(t, env, strings.Repeat("a", 32))
	if code != 1 || len(records) != 0 || !strings.Contains(errOut, "Not set up") {
		t.Fatalf("exit %d, stderr %q, records %v", code, errOut, records)
	}
}

// A malformed HTTPS URL must produce a safe per-input error at either detail.
func TestEvalExportRefusesMalformedGitURL(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	key, err := archive.MetadataObjectKey("codex", id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := mem.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var sidecar map[string]any
	if err := json.Unmarshal(raw, &sidecar); err != nil {
		t.Fatal(err)
	}
	const badURL = "https://bad host/private-token"
	sidecar["git_activity"] = []any{map[string]any{"kind": "push", "source": "shell", "url": badURL}}
	raw, err = json.Marshal(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Put(context.Background(), key, raw); err != nil {
		t.Fatal(err)
	}
	for _, detail := range []string{"metadata", "full"} {
		records, errOut, code := evalLines(t, env, "--detail", detail, id)
		if code != 1 || len(records) != 1 || records[0]["record"] != "error" || records[0]["error"].(map[string]any)["code"] != "read_failed" {
			t.Errorf("--detail %s: exit %d, %v", detail, code, records)
		}
		encoded, err := json.Marshal(records)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded)+errOut, "private-token") || strings.Contains(string(encoded)+errOut, "bad host") {
			t.Errorf("--detail %s: malformed URL leaked", detail)
		}
	}
}
