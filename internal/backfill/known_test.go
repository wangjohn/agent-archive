package backfill

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

// Setup offers the projects sessions ran in, most recently used first,
// leaving out configured projects, home, temporary folders, and folders
// that are gone.
func TestKnownProjectsNewestFirstWithoutConfiguredOrUnusable(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	older, newer, configured := tr.repo("home/older"), tr.repo("home/newer"), tr.repo("home/configured")
	start := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	touch := func(path string, at time.Time) {
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	touch(tr.write(filepath.Join("home", claudeFile("older", "c1")), claudeTranscript("c1", older, start)), start)
	touch(tr.write(filepath.Join("home", claudeFile("older", "c2")), claudeTranscript("c2", filepath.Join(older, "sub"), start)), start.Add(time.Hour))
	const codexID = "0a9b3c4d-0000-4000-8000-00000000000a"
	touch(tr.write(filepath.Join("home", codexFile(codexID)), codexTranscript(codexID, "", newer, start)), start.Add(2*time.Hour))
	tr.write(filepath.Join("home", claudeFile("configured", "c3")), claudeTranscript("c3", configured, start))
	tr.write(filepath.Join("home", claudeFile("home", "c4")), claudeTranscript("c4", tr.home, start))
	tr.write(filepath.Join("home", claudeFile("tmp", "c5")), claudeTranscript("c5", tr.mkdir("tmp/x"), start))
	tr.write(filepath.Join("home", claudeFile("gone", "c6")), claudeTranscript("c6", tr.path("home/gone"), start))

	cfg := config.Config{}
	cfg.Archive.Projects = append(cfg.Archive.Projects, project(configured, false))
	got, err := KnownProjects(context.Background(), tr.env(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Root != newer || got[1].Root != older || got[1].Sessions != 2 || !got[1].LastUsed.Equal(start.Add(time.Hour)) {
		t.Fatalf("got %+v", got)
	}
}
