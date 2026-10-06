package backfill

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

func TestBoundedProjectsReadOnlyFirstRecordAndPreserveCapResults(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	a, b := tr.repo("home/a"), tr.repo("home/b")
	record := func(cwd string) string {
		data, _ := json.Marshal(map[string]string{"cwd": cwd})
		return string(data) + "\n"
	}
	tr.write(filepath.Join("home", claudeFile("a", "1")), record(a)+record(b))
	tr.write(filepath.Join("home", claudeFile("b", "2")), record(b))
	got := KnownProjectsBounded(context.Background(), tr.env(), config.Config{}, 1)
	if len(got.Projects) != 1 || got.Projects[0].Root != a || !got.Capped {
		t.Fatalf("got %+v", got)
	}
	got = KnownProjectsBounded(context.Background(), tr.env(), config.Config{}, 128)
	if got.Incomplete() || len(got.Projects) != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestBoundedProjectsCancellationDuringEnumerationKeepsPartialResults(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	a := tr.repo("home/a")
	data, _ := json.Marshal(map[string]string{"cwd": a})
	tr.write(filepath.Join("home", claudeFile("a", "1")), string(data)+"\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := tr.env()
	env.ReadDir = func(path string) ([]fs.DirEntry, error) {
		if filepath.Base(path) == "sessions" {
			cancel()
			return nil, errors.New("cancelled")
		}
		return os.ReadDir(path)
	}
	got := KnownProjectsBounded(ctx, env, config.Config{}, 128)
	if !got.TimedOut || len(got.Projects) != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestBoundedProjectsDoNotSearchBodiesAndReportUnreadableHeaders(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	root := tr.repo("home/repo")
	data, _ := json.Marshal(map[string]string{"cwd": root})
	tr.write(filepath.Join("home", claudeFile("body", "1")), "{}\n"+string(data)+"\n")
	tr.write(filepath.Join("home", claudeFile("bad", "2")), "invalid\n"+string(data)+"\n")
	got := KnownProjectsBounded(context.Background(), tr.env(), config.Config{}, 128)
	if len(got.Projects) != 0 || got.Unreadable != 1 || !got.Incomplete() {
		t.Fatalf("got %+v", got)
	}
}

type projectCancelReader struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r projectCancelReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.cancel()
	return n, err
}

func TestBoundedProjectsCancelDuringHeaderReadKeepsEarlierRoots(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	root := tr.repo("home/repo")
	data, _ := json.Marshal(map[string]string{"cwd": root})
	tr.write(filepath.Join("home", claudeFile("a", "1")), string(data)+"\n")
	tr.write(filepath.Join("home", claudeFile("b", "2")), string(data)+"\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := tr.env()
	env.Open = func(path string) (io.ReadCloser, error) {
		if strings.HasSuffix(path, "2.jsonl") {
			return projectCancelReader{ReadCloser: io.NopCloser(strings.NewReader(string(data) + "\n")), cancel: cancel}, nil
		}
		return os.Open(path)
	}
	got := KnownProjectsBounded(ctx, env, config.Config{}, 128)
	if !got.TimedOut || len(got.Projects) != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestBoundedProjectsAlreadyCancelledDoesNoFilesystemWork(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	env := Environment{EvalSymlinks: func(string) (string, error) { t.Fatal("resolved after cancellation"); return "", nil }, ReadDir: func(string) ([]fs.DirEntry, error) { t.Fatal("enumerated after cancellation"); return nil, nil }}
	if got := KnownProjectsBounded(ctx, env, config.Config{}, 128); !got.TimedOut {
		t.Fatalf("got %+v", got)
	}
}

func TestBoundedProjectsCapsEnumerationWithoutTranscriptFiles(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	var entries []fs.DirEntry
	for i := range 8193 {
		entries = append(entries, projectNonTranscriptEntry{name: strconv.Itoa(i)})
	}
	env := tr.env()
	env.ReadDir = func(path string) ([]fs.DirEntry, error) {
		if filepath.Base(path) == "projects" {
			return entries, nil
		}
		return nil, nil
	}
	got := KnownProjectsBounded(context.Background(), env, config.Config{}, 128)
	if !got.Capped || len(got.Projects) != 0 {
		t.Fatalf("got %+v", got)
	}
}

type projectNonTranscriptEntry struct{ name string }

func (e projectNonTranscriptEntry) Name() string { return e.name }

func (projectNonTranscriptEntry) IsDir() bool { return false }

func (projectNonTranscriptEntry) Type() fs.FileMode { return 0 }

func (projectNonTranscriptEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrNotExist }

func TestBoundedProjectsLimitsEntryProcessingBeforeConversion(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	calls := 0
	entries := make([]fs.DirEntry, 9000)
	for i := range entries {
		entries[i] = projectCountedEntry{onName: func() { calls++ }}
	}
	env := tr.env()
	env.ReadDir = func(string) ([]fs.DirEntry, error) { return entries, nil }
	got := KnownProjectsBounded(t.Context(), env, config.Config{}, 128)
	if !got.Capped || calls > 8192 {
		t.Fatalf("got %+v; processed %d entries", got, calls)
	}
}

type projectCountedEntry struct{ onName func() }

func (e projectCountedEntry) Name() string {
	e.onName()
	return "ignored"
}

func (projectCountedEntry) IsDir() bool { return false }

func (projectCountedEntry) Type() fs.FileMode { return fs.ModeSymlink }

func (projectCountedEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrNotExist }

func TestBoundedProjectsStopsProcessingEntriesOnCancellation(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	env := tr.env()
	env.ReadDir = func(string) ([]fs.DirEntry, error) {
		return []fs.DirEntry{
			projectCountedEntry{onName: func() { calls++; cancel() }},
			projectCountedEntry{onName: func() { calls++ }},
		}, nil
	}
	got := KnownProjectsBounded(ctx, env, config.Config{}, 128)
	if !got.TimedOut || calls != 1 {
		t.Fatalf("got %+v; processed %d entries", got, calls)
	}
}

func TestBoundedProjectsRecordsCancellationAfterLastEmptyDirectoryRead(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	env := tr.env()
	env.ReadDir = func(path string) ([]fs.DirEntry, error) {
		if filepath.Base(path) == "archived_sessions" {
			cancel()
		}
		return nil, nil
	}
	got := KnownProjectsBounded(ctx, env, config.Config{}, 128)
	if !got.TimedOut {
		t.Fatalf("got %+v", got)
	}
}

func TestBoundedProjectsKeepsSameNamedNativeCopiesAcrossStores(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	a, b := tr.repo("home/a"), tr.repo("home/b")
	record := func(cwd string) string {
		raw, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"cwd": cwd}})
		return string(raw) + "\n"
	}
	tr.write("home/.codex/sessions/day/rollout-same.jsonl", record(a))
	tr.write("home/.codex/archived_sessions/rollout-same.jsonl", record(b))
	got := KnownProjectsBounded(t.Context(), tr.env(), config.Config{}, 128)
	if got.Incomplete() || len(got.Projects) != 2 {
		t.Fatalf("native copies hid repository roots: %+v", got)
	}
}

func TestBoundedProjectsEntryCapKeepsEarlierRoot(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	root := tr.repo("home/repo")
	raw, _ := json.Marshal(map[string]string{"cwd": root})
	tr.write("home/.claude/projects/0.jsonl", string(raw)+"\n")
	env := tr.env()
	env.ReadDir = func(path string) ([]fs.DirEntry, error) {
		entries, err := os.ReadDir(path)
		if err != nil || filepath.Base(path) != "projects" {
			return entries, err
		}
		for range 8193 {
			entries = append(entries, projectNonTranscriptEntry{name: "z-ignored"})
		}
		return entries, nil
	}
	got := KnownProjectsBounded(t.Context(), env, config.Config{}, 128)
	if !got.Capped || len(got.Projects) != 1 || got.Projects[0].Root != root {
		t.Fatalf("entry cap discarded usable prefix: %+v", got)
	}
}

func TestBoundedProjectsIgnoreUnrelatedDeepNativeRoots(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		t.Run(strconv.FormatBool(reverse), func(t *testing.T) {
			t.Parallel()
			tr := newTree(t)
			root := tr.repo("home/repo")
			raw, _ := json.Marshal(map[string]string{"cwd": root})
			tr.write("home/.claude/projects/0.jsonl", string(raw)+"\n")
			deep := filepath.Join(tr.root, "unrelated", strings.Repeat("nested/", 40))
			env := tr.env()
			bases := []string{filepath.Join(env.Home, ".claude"), deep}
			if reverse {
				bases[0], bases[1] = bases[1], bases[0]
			}
			env.NativeDirectories = map[string][]string{"claude": bases}
			got := KnownProjectsBounded(t.Context(), env, config.Config{}, 128)
			if got.Incomplete() || len(got.Projects) != 1 || got.Projects[0].Root != root {
				t.Fatalf("unrelated native root hid readable history: %+v", got)
			}
		})
	}
}

func TestBoundedProjectDirectoryDepthUsesClosestContainingRoot(t *testing.T) {
	t.Parallel()
	base := filepath.Join(string(filepath.Separator), "synthetic", "native")
	nested := filepath.Join(base, strings.Repeat("nested/", 40))
	for _, tc := range []struct {
		name   string
		bases  []string
		path   string
		capped bool
	}{
		{name: "depth33", bases: []string{base}, path: filepath.Join(base, strings.Repeat("child/", 33))},
		{name: "depth34", bases: []string{base}, path: filepath.Join(base, strings.Repeat("child/", 34)), capped: true},
		{name: "nested_root_last", bases: []string{base, nested}, path: filepath.Join(nested, "projects")},
		{name: "nested_root_first", bases: []string{nested, base}, path: filepath.Join(nested, "projects")},
		{name: "sibling_prefix", bases: []string{base}, path: filepath.Join(base+"-other", strings.Repeat("child/", 34))},
		{name: "parent", bases: []string{nested}, path: base},
		{name: "rel_error", bases: []string{"relative"}, path: base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reads := 0
			d := projectDiscovery{ctx: t.Context(), env: Environment{ReadDir: func(string) ([]fs.DirEntry, error) { reads++; return nil, nil }}}
			files := projectFiles{d: &d, bases: tc.bases}
			_, err := files.ReadDir(tc.path)
			if d.result.Capped != tc.capped || errors.Is(err, errProjectLimit) != tc.capped || reads != boolReadCount(tc.capped) {
				t.Fatalf("depth boundary: capped=%v reads=%d err=%v", d.result.Capped, reads, err)
			}
		})
	}
}

func boolReadCount(capped bool) int {
	if capped {
		return 0
	}
	return 1
}

func TestBoundedProjectsAggregatesObservedSessionsAndLatestUse(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	root := tr.repo("home/project")
	start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		path := tr.write(filepath.Join("home", claudeFile("project", strconv.Itoa(i))), claudeTranscript(strconv.Itoa(i), root, start))
		if err := os.Chtimes(path, start.Add(time.Duration(i)*time.Hour), start.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	got := KnownProjectsBounded(t.Context(), tr.env(), config.Config{}, 128)
	if got.Incomplete() || len(got.Projects) != 1 || got.Projects[0].Sessions != 2 || !got.Projects[0].LastUsed.Equal(start.Add(time.Hour)) {
		t.Fatalf("evidence %+v", got)
	}
	tr.write(filepath.Join("home", claudeFile("project", "broken")), "broken header\n")
	got = KnownProjectsBounded(t.Context(), tr.env(), config.Config{}, 128)
	if !got.Incomplete() || got.Unreadable == 0 || got.Projects[0].Sessions != 2 {
		t.Fatalf("partial evidence %+v", got)
	}
}
