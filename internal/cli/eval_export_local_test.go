package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// localEvalFixture is backfill's fixture of transcripts on a Mac, with its
// data directory pointed at one that does not exist: local export needs no
// setup, and must not create it.
func localEvalFixture(t *testing.T) *backfillFixture {
	t.Helper()
	f := newBackfillFixture(t)
	missing := filepath.Join(f.root, "no-data-directory")
	f.env.Home = func() (string, error) { return missing, nil }
	f.data = missing
	return f
}

func (f *backfillFixture) evalLines(t *testing.T, stdin string, args ...string) ([]map[string]any, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(append([]string{"eval", "export"}, args...), strings.NewReader(stdin), &out, &errOut, f.env)
	records := decodeEvalLines(t, out.String())
	if _, err := os.Stat(f.data); !os.IsNotExist(err) {
		t.Fatalf("eval export %v created the data directory %s (%v)", args, f.data, err)
	}
	return records, errOut.String(), code
}

// recordsBy indexes records by the field that names their input.
func recordsBy(records []map[string]any, field string) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, r := range records {
		key, _ := r[field].(string)
		if key == "" {
			key, _ = r["input"].(string)
		}
		out[key] = r
	}
	return out
}

// --file exports one transcript with no setup: the app's own session ID, the
// transcript's path, and the project it ran in, with the fields only a hook
// records (git_head, replay, feedback) and the archive's (machine_id,
// captured_at) left out rather than guessed.
func TestEvalExportFileNeedsNoSetup(t *testing.T) {
	t.Parallel()
	f := localEvalFixture(t)
	path := filepath.Join(f.userHome, ".claude", "projects", "slug-c-lev-1", "c-lev-1.jsonl")
	records, errOut, code := f.evalLines(t, "", "--file", path, "--harness", "claude")
	if code != 0 || errOut != "" || len(records) != 1 {
		t.Fatalf("exit %d, stderr %q, records %v", code, errOut, records)
	}
	r := records[0]
	project, _ := r["project"].(map[string]any)
	if r["source"] != "local" || r["session_id"] != "c-lev-1" || r["transcript_path"] != path ||
		project["root"] != filepath.Join(f.userHome, "levenshtein") || project["name"] != "levenshtein" || r["started_at"] != "2026-09-17T20:00:00Z" {
		t.Errorf("record = %v", r)
	}
	for _, key := range []string{"git_head", "replay", "feedback", "machine_id", "captured_at"} {
		if _, found := r[key]; found {
			t.Errorf("a local record has %s: %v", key, r)
		}
	}
	if prompts, _ := r["prompts"].([]any); len(prompts) != 1 {
		t.Errorf("prompts = %v", r["prompts"])
	}
	if _, errOut, code := f.evalLines(t, "", "--file", path); code != 2 || !strings.Contains(errOut, "--file requires --harness") {
		t.Errorf("--file without --harness: exit %d, %q", code, errOut)
	}
	records, _, code = f.evalLines(t, "", "--file", filepath.Join(f.userHome, "missing.jsonl"), "--harness", "claude")
	if code != 1 || len(records) != 1 || records[0]["record"] != "error" || records[0]["error"].(map[string]any)["code"] != "not_found" {
		t.Errorf("missing file: exit %d, %v", code, records)
	}
}

// --scan exports what backfill would import from this machine, skipping what
// it would skip (the home folder, a temporary folder, an empty transcript),
// with backfill's filters, several at a time, and never the Cursor chats
// only its database holds.
func TestEvalExportScanFindsWhatBackfillWouldImport(t *testing.T) {
	t.Parallel()
	f := localEvalFixture(t)
	records, errOut, code := f.evalLines(t, "", "--scan", "--detail", "metadata", "--workers", "3")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	byID := recordsBy(records, "session_id")
	for _, want := range []string{"c-aa-1", "c-lev-1", "c-lev-2", "c-archived", "0a9b3c4d-0000-4000-8000-0000000000aa", "k-aa"} {
		r, found := byID[want]
		if !found {
			t.Errorf("--scan did not export %s; got %v", want, keys(byID))
			continue
		}
		if r["source"] != "local" || r["detail"] != "metadata" || r["transcript_path"] == nil {
			t.Errorf("%s: %v", want, r)
		}
		if _, found := r["prompts"]; found {
			t.Errorf("%s: a metadata record has prompts", want)
		}
	}
	for _, skipped := range []string{"c-tmp-1", "c-home", "c-empty", "k-db-only"} {
		if _, found := byID[skipped]; found {
			t.Errorf("--scan exported %s, which backfill skips", skipped)
		}
	}
	if len(records) != len(byID) {
		t.Errorf("%d records for %d sessions: one was written twice", len(records), len(byID))
	}
	filtered, _, code := f.evalLines(t, "", "--scan", "--harness", "claude", "--since", "2026-09-20", "--project", filepath.Join(f.userHome, "levenshtein"))
	ids := keys(recordsBy(filtered, "session_id"))
	if code != 0 || strings.Join(ids, " ") != "c-lev-3" {
		t.Errorf("filtered scan = %v (exit %d), want c-lev-3", ids, code)
	}
	if _, errOut, code := f.evalLines(t, "", "--since", "2026-09-20", "--file", "x"); code != 2 || !strings.Contains(errOut, "apply only to --scan") {
		t.Errorf("--since without --scan: exit %d, %q", code, errOut)
	}
}

// --ids-from - reads archive IDs and transcript paths, one a line, and
// works out which app wrote a transcript from the folder it is in.
func TestEvalExportReadsIDsAndPathsFromStdin(t *testing.T) {
	t.Parallel()
	f := localEvalFixture(t)
	known := filepath.Join(f.userHome, ".codex", "sessions", "2026", "09", "20", "rollout-2026-09-20T10-00-00-0a9b3c4d-0000-4000-8000-0000000000aa.jsonl")
	stray := filepath.Join(f.root, "stray.jsonl")
	if err := os.WriteFile(stray, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin := known + "\n\n" + stray + "\n"
	records, errOut, code := f.evalLines(t, stdin, "--ids-from", "-", "--workers", "2")
	if code != 1 || errOut != "" || len(records) != 2 {
		t.Fatalf("exit %d, stderr %q, records %v", code, errOut, records)
	}
	byPath := recordsBy(records, "transcript_path")
	if r := byPath[known]; r["record"] != "session" || r["harness"].(map[string]any)["name"] != "codex" {
		t.Errorf("codex transcript: %v", r)
	}
	if r := byPath[stray]; r["record"] != "error" || r["error"].(map[string]any)["code"] != "unknown_harness" {
		t.Errorf("stray transcript: %v", r)
	}
	// An archive ID among the lines needs setup, which this Mac does not have.
	_, errOut, code = f.evalLines(t, strings.Repeat("a", 32)+"\n", "--ids-from", "-")
	if code != 1 || !strings.Contains(errOut, "Not set up") {
		t.Errorf("archive ID without setup: exit %d, %q", code, errOut)
	}
	for _, args := range [][]string{{"--ids-from", "ids.txt"}, {"--ids-from", "-", strings.Repeat("a", 32)}, {"--scan", "--file", known}, {"--workers", "-1", "--scan"}} {
		if _, errOut, code := f.evalLines(t, "", args...); code != 2 || errOut == "" {
			t.Errorf("%v: exit %d, %q", args, code, errOut)
		}
	}
}

// With several workers every input still gets exactly one whole line.
func TestEvalExportWorkersWriteEachRecordOnce(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	args := []string{"--workers", "4"}
	for range 12 {
		args = append(args, id)
	}
	args = append(args, strings.Repeat("0", 32))
	records, _, code := evalLines(t, env, args...)
	sessions, errors := 0, 0
	for _, r := range records {
		switch r["record"] {
		case "session":
			sessions++
		case "error":
			errors++
		}
	}
	if code != 1 || sessions != 12 || errors != 1 {
		t.Errorf("exit %d, %d sessions and %d errors in %d records", code, sessions, errors, len(records))
	}
}

func keys(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// --ids-from - reads a list piped in; on a terminal it would wait for a
// person, so it is refused.
func TestEvalExportIDsFromRefusesATerminal(t *testing.T) {
	t.Parallel()
	f := localEvalFixture(t)
	f.env.IsTerminal = func(stream any) bool { return true }
	records, errOut, code := f.evalLines(t, "", "--ids-from", "-")
	if code != 2 || len(records) != 0 || !strings.Contains(errOut, "not a terminal") {
		t.Errorf("exit %d, stderr %q, records %v", code, errOut, records)
	}
}
