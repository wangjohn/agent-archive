package backfill

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

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
