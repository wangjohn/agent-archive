package cli

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

var updateBackfillGolden = flag.Bool("update", false, "rewrite internal/cli/testdata/backfill golden files")

// backfillNow is the planning clock: 2026-09-23 in California.
var backfillNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.FixedZone("PDT", -7*3600))

// backfillFixture is a Mac with history in all three apps. Every transcript
// is synthetic, shaped like internal/archive/testdata's fixtures.
type backfillFixture struct {
	env                  Env
	root, userHome, data string
	// nativeIDs are every native session ID in the tree; none may be printed.
	nativeIDs []string
}

func newBackfillFixture(t *testing.T) *backfillFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &backfillFixture{root: root, userHome: filepath.Join(root, "home"), data: filepath.Join(root, "data")}
	for _, dir := range []string{f.userHome, f.data, filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	repo := func(name string) string {
		dir := filepath.Join(f.userHome, name)
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	agentArchive, levenshtein, website := repo("agent-archive"), repo("levenshtein"), repo("personal-website")
	scratch := filepath.Join(f.userHome, "Library", "Application Support", "Claude", "scratch-workspaces", "a1-b2", "scratch-2026-09-21")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		MachineID:     "machine-1",
		Storage:       credentials.Config{Provider: credentials.ProviderS3, Bucket: "personal-agent-archive", Region: "us-east-1", AWSProfile: "test", Prefix: "agent-archive/"},
		Harnesses:     []string{"claude"},
		RetentionDays: 90,
		Archive: archive.Config{SchemaVersion: 1, MachineID: "machine-1", Enabled: true, Projects: []archive.ProjectActivation{
			{ProjectID: archive.ProjectID(agentArchive), Root: agentArchive, Included: true, ActivatedAt: time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)},
		}},
	}
	if err := config.Save(f.data, cfg); err != nil {
		t.Fatal(err)
	}

	day := func(d, h int) time.Time { return time.Date(2026, 9, d, h, 0, 0, 0, time.UTC) }
	claude := func(id, cwd string, start time.Time) string {
		f.nativeIDs = append(f.nativeIDs, id)
		path := f.write(t, filepath.Join(".claude", "projects", "slug-"+id, id+".jsonl"), fmt.Sprintf(`{"type":"user","uuid":"a","sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"role":"user","content":"please check it"}}
{"type":"assistant","uuid":"b","sessionId":%q,"timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":"Checked."}]}}
`, id, cwd, start.Format(time.RFC3339), id, start.Add(time.Minute).Format(time.RFC3339)))
		return path
	}
	claude("c-aa-1", agentArchive, day(19, 9))
	claude("c-aa-2", filepath.Join(agentArchive, ".claude", "worktrees", "gone"), day(21, 9))
	for _, agent := range []string{"agent-a1", "agent-a2"} {
		f.write(t, filepath.Join(".claude", "projects", "slug-c-aa-2", "c-aa-2", "subagents", agent+".jsonl"), `{"type":"user","sessionId":"c-aa-2","timestamp":"2026-09-21T09:05:00Z","message":{"role":"user","content":"look"}}`+"\n")
	}
	claude("c-lev-1", levenshtein, day(17, 20))
	claude("c-lev-2", filepath.Join(levenshtein, "src"), day(18, 9))
	claude("c-lev-3", levenshtein, day(20, 9))
	claude("c-scratch-1", scratch, day(21, 10))
	claude("c-scratch-2", filepath.Join(filepath.Dir(scratch), "scratch-2026-09-22"), day(22, 10))
	claude("c-old", filepath.Join(f.userHome, "old-notes"), day(18, 12))
	claude("c-archived", agentArchive, day(22, 12))
	claude("c-tmp-1", filepath.Join(root, "tmp", "run-1"), day(20, 1))
	claude("c-tmp-2", filepath.Join(root, "tmp", "run-2"), day(20, 2))
	claude("c-home", f.userHome, day(20, 3))
	f.nativeIDs = append(f.nativeIDs, "c-empty")
	f.write(t, filepath.Join(".claude", "projects", "slug-empty", "c-empty.jsonl"), `{"type":"summary","summary":"recap","cwd":"`+agentArchive+`"}`+"\n")

	const codexID = "0a9b3c4d-0000-4000-8000-0000000000aa"
	f.nativeIDs = append(f.nativeIDs, codexID)
	f.write(t, filepath.Join(".codex", "sessions", "2026", "09", "20", "rollout-2026-09-20T10-00-00-"+codexID+".jsonl"), fmt.Sprintf(`{"type":"session_meta","timestamp":"2026-09-20T17:00:00Z","payload":{"id":%q,"timestamp":"2026-09-20T17:00:00Z","cwd":%q,"source":"cli"}}
{"type":"response_item","timestamp":"2026-09-20T17:00:05Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect the file"}]}}
`, codexID, agentArchive))

	cursor := func(id, folder string) {
		f.nativeIDs = append(f.nativeIDs, id)
		path := f.write(t, filepath.Join(".cursor", "projects", cursorSlugFor(folder), "agent-transcripts", id, id+".jsonl"), `{"role":"user","message":{"content":[{"type":"text","text":"Summarize the failing widget test."}]}}
`)
		// Setting an earlier modification time moves the birth time back with
		// it on macOS; elsewhere the modification time is the fallback.
		created := time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC)
		if err := os.Chtimes(path, created, created); err != nil {
			t.Fatal(err)
		}
	}
	cursor("k-aa", agentArchive)
	cursor("k-site", website)
	cursor("k-lost", filepath.Join(f.userHome, "no-such-folder"))

	// c-archived is already registered by a hook.
	store, err := collector.NewLocalStore(f.data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterNewSession("c-archived", func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: "c-archived", ProjectID: archive.ProjectID(agentArchive), ProjectRoot: agentArchive,
			Harness: archive.Harness{Name: "claude"}, SessionStartedAt: day(22, 12), RegisteredAt: day(22, 12)}
	}); err != nil {
		t.Fatal(err)
	}

	f.env = Env{
		UserHomeDir:      func() (string, error) { return f.userHome, nil },
		Home:             func() (string, error) { return f.data, nil },
		Now:              func() time.Time { return backfillNow },
		LookupEnv:        func(string) (string, bool) { return "", false },
		BackfillTempDirs: []string{filepath.Join(root, "tmp")},
	}
	return f
}

// cursorSlugFor is Cursor's folder name for a workspace path.
func cursorSlugFor(path string) string {
	b := []byte(strings.TrimPrefix(path, "/"))
	for i, c := range b {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// rootPad is the length every occurrence of the fixture's root is padded to,
// so file sizes, and the plan's byte counts, do not depend on where
// t.TempDir happens to be.
const rootPad = 400

func (f *backfillFixture) write(t *testing.T, rel, content string) string {
	t.Helper()
	if n := strings.Count(content, f.root); n > 0 {
		if len(f.root) >= rootPad {
			t.Fatalf("temporary directory path longer than %d bytes", rootPad)
		}
		pad := `{"pad":"` + strings.Repeat("x", n*(rootPad-len(f.root))) + `",`
		content = strings.Replace(content, "{", pad, 1)
	}
	path := filepath.Join(f.userHome, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f *backfillFixture) run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(append([]string{"backfill"}, args...), nil, &out, &errOut, f.env)
	return out.String(), errOut.String(), code
}

// checkGolden compares output with testdata/backfill/name. Regenerate with
// `go test ./internal/cli -run TestBackfillGolden -update` and review the
// diff.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	golden := filepath.Join("testdata", "backfill", name)
	if *updateBackfillGolden {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("output differs from %s:\n%s", golden, got)
	}
}

func TestBackfillGolden(t *testing.T) {
	f := newBackfillFixture(t)
	cases := []struct {
		name string
		args []string
	}{
		{"default.txt", []string{"--dry-run"}},
		// Temporary folders lie outside home, so their rows would show the
		// temporary root; --include-home keeps every row under ~.
		{"filtered.txt", []string{"--dry-run", "--harness", "claude", "--since", "2026-09-19", "--include-home"}},
		{"plan.json", []string{"--dry-run", "--json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut, code := f.run(t, tc.args...)
			if code != 0 {
				t.Fatalf("code %d, stderr %s", code, errOut)
			}
			for _, id := range f.nativeIDs {
				if strings.Contains(out, id) {
					t.Errorf("output names native session %q", id)
				}
			}
			if strings.Contains(out, ".jsonl") || strings.Contains(out, "please check it") {
				t.Error("output names a transcript or its content")
			}
			checkGolden(t, tc.name, []byte(strings.ReplaceAll(out, f.root, "$ROOT")))
		})
	}
}

// A dry run writes nothing: the data directory is byte-for-byte unchanged.
func TestBackfillDryRunWritesNothing(t *testing.T) {
	f := newBackfillFixture(t)
	before := snapshotTree(t, f.data)
	if _, errOut, code := f.run(t, "--dry-run"); code != 0 {
		t.Fatalf("code %d: %s", code, errOut)
	}
	if after := snapshotTree(t, f.data); after != before {
		t.Fatalf("data directory changed:\n%s\n---\n%s", before, after)
	}
}

func snapshotTree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %d %s\n", strings.TrimPrefix(path, dir), info.Size(), info.ModTime().Format(time.RFC3339Nano))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestBackfillRefusals(t *testing.T) {
	f := newBackfillFixture(t)

	if _, errOut, code := f.run(t); code != 1 || !strings.Contains(errOut, "needs a terminal") {
		t.Fatalf("import: code %d, %q", code, errOut)
	}
	for _, args := range [][]string{{"--json"}, {"--dry-run", "--harness", "vim"}, {"--dry-run", "--since", "yesterday"}, {"--dry-run", "extra"}, {"--dry-run", "--background"}, {"history", "extra"}} {
		if _, _, code := f.run(t, args...); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}
	if _, errOut, code := f.run(t, "undo"); code != 1 || !strings.Contains(errOut, "needs a terminal") {
		t.Fatalf("undo: code %d, %q", code, errOut)
	}
	for _, args := range [][]string{{"undo", "a", "b"}, {"undo", "--bogus"}} {
		if _, _, code := f.run(t, args...); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}

	// --yes is harmless with --dry-run.
	if out, errOut, code := f.run(t, "--dry-run", "--yes"); code != 0 || !strings.Contains(out, "Dry run: nothing was changed.") {
		t.Fatalf("--dry-run --yes: code %d, stderr %q", code, errOut)
	}

	// A dry run works while paused and while a setup transaction is pending.
	if _, err := config.SetPaused(f.data, true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalPath(f.data), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, errOut, code := f.run(t, "--dry-run"); code != 0 || !strings.Contains(out, "Dry run: nothing was changed.") {
		t.Fatalf("paused dry run: code %d, stderr %q", code, errOut)
	}

	notSetUp := testEnv(t, t.TempDir(), backfillNow)
	var out, errOut bytes.Buffer
	if code := Run([]string{"backfill", "--dry-run"}, nil, &out, &errOut, notSetUp); code != 1 || !strings.Contains(errOut.String(), "not set up") {
		t.Fatalf("not set up: code %d, %q", code, errOut.String())
	}
}

func TestBackfillHelp(t *testing.T) {
	var out bytes.Buffer
	if code := Run([]string{"backfill", "--help"}, nil, &out, nil, Env{}); code != 0 || !strings.Contains(out.String(), "Usage: agent-archive backfill") {
		t.Fatalf("code %d: %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"help", "backfill"}, nil, &out, nil, Env{}); code != 0 || !strings.Contains(out.String(), "--dry-run") {
		t.Fatalf("code %d: %s", code, out.String())
	}
}

// Classify reports a registration the configuration no longer accepts as
// registered_not_admitted.
func TestBackfillArchiveState(t *testing.T) {
	f := newBackfillFixture(t)
	cfg, _, err := config.Load(f.data)
	if err != nil {
		t.Fatal(err)
	}
	state := newArchiveState(f.data, cfg)
	if got, err := state.Classify("claude", "c-archived"); err != nil || got != "already_archived" {
		t.Fatalf("accepted: %q, %v", got, err)
	}
	if got, err := state.Classify("claude", "c-lev-1"); err != nil || got != "" {
		t.Fatalf("unknown: %q, %v", got, err)
	}
	cfg.Archive.Projects[0].Included = false
	if got, err := newArchiveState(f.data, cfg).Classify("claude", "c-archived"); err != nil || got != "registered_not_admitted" {
		t.Fatalf("not admitted: %q, %v", got, err)
	}
}
