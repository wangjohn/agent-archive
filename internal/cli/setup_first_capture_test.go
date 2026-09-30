package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
)

// Without a repository to pre-select, or without an app setup knows, setup
// keeps its separate questions.
func TestOfferFirstCaptureAsksNothingWhenItCannotGuessBoth(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		detect  []string
		current string
	}{
		{"no repository", []string{"claude"}, ""},
		{"no apps", nil, "/Users/alex/src/app"},
		{"only apps setup does not know", []string{"windsurf"}, "/Users/alex/src/app"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			p := newPrompter(strings.NewReader(""), &out)
			cfg := config.Config{}
			done, err := offerFirstCapture(p, &cfg, tc.detect, tc.current, "/Users/alex", nil)
			if done || err != nil || out.Len() != 0 || len(cfg.Harnesses) != 0 || len(cfg.Archive.Projects) != 0 {
				t.Fatalf("done=%v err=%v cfg=%+v asked:\n%s", done, err, cfg, &out)
			}
		})
	}
}

// The repository setup was run from heads the project list, unless it is too
// broad to archive on one Enter: the home folder, a folder holding it, or the
// temporary folder or anything in it, however far up the repository is. Then
// there is no current project, and nothing is pre-selected.
func TestCurrentProjectIsNeverTheHomeAncestorOfItOrTheTempDir(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// gitAt lists folders under the case's root that hold a .git.
		gitAt []string
		// cwd is where setup runs, under the root.
		cwd string
		// want is the project's folder under the root, or "" for none.
		want string
	}{
		{"home folder", []string{"home"}, "home", ""},
		{"inside the home folder, repository above it", []string{"."}, "home/src/app", ""},
		{"folder holding home", []string{"."}, ".", ""},
		{"temporary folder", []string{"tmp"}, "tmp", ""},
		{"inside the temporary folder", []string{"tmp/scratch/app"}, "tmp/scratch/app", ""},
		{"ordinary repository", []string{"work/app"}, "work/app/pkg", "work/app"},
		{"no repository", nil, "work/app", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, err := filepath.EvalSymlinks(t.TempDir())
			must(t, err)
			at := func(rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }
			for _, dir := range []string{"home/src/app", "tmp/scratch/app", "work/app/pkg"} {
				must(t, os.MkdirAll(at(dir), 0o700))
			}
			for _, dir := range tc.gitAt {
				must(t, os.MkdirAll(filepath.Join(at(dir), ".git"), 0o700))
			}
			env := Env{WorkingDir: func() (string, error) { return at(tc.cwd), nil }, TempDir: func() string { return at("tmp") }}
			want := ""
			if tc.want != "" {
				want = at(tc.want)
			}
			if got := currentProject(env, at("home")); got != want {
				t.Fatalf("currentProject = %q, want %q", got, want)
			}
		})
	}
}

// A folder next to the home folder or the temporary folder, not holding or
// inside them, is not broad.
func TestBroadFolderIgnoresNeighbors(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"/Users/alex/src/app", "/Users/alexandra", "/private/var/folders/xy/Tools/app"} {
		if broadFolder(dir, "/Users/alex", "/private/var/folders/xy/T") {
			t.Errorf("broadFolder(%s) = true", dir)
		}
	}
}

// The answer guesses an ordinary folder.
func TestOfferFirstCaptureGuessesTheCurrentProject(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("\n"), &out)
	cfg := config.Config{}
	done, err := offerFirstCapture(p, &cfg, []string{"claude"}, "/Users/alex/src/app", "/Users/alex", nil)
	if !done || err != nil || len(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Root != "/Users/alex/src/app" || len(cfg.Harnesses) != 1 {
		t.Fatalf("done=%v err=%v cfg=%+v\n%s", done, err, cfg, &out)
	}
}

// Folders are compared with their symlinks resolved, as project roots are
// saved: a temporary folder reached through a link is still the temporary
// folder.
func TestBroadFolderResolvesSymlinks(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	target := filepath.Join(root, "target")
	must(t, os.MkdirAll(filepath.Join(target, "app"), 0o700))
	link := filepath.Join(root, "link")
	must(t, os.Symlink(target, link))
	if !broadFolder(filepath.Join(target, "app"), "/Users/alex", link) {
		t.Fatal("a folder inside the temporary folder, named through a link, was taken for ordinary")
	}
	if !broadFolder(link, filepath.Join(target, "app"), "/nowhere") {
		t.Fatal("a link to a folder holding the home folder was taken for ordinary")
	}
}

// The answer says how many other projects the apps' history holds, and the
// review repeats where to add them.
func TestOfferFirstCaptureNamesOtherProjects(t *testing.T) {
	t.Parallel()
	known := func(config.Config) []backfill.KnownProject {
		return []backfill.KnownProject{{Root: "/Users/alex/src/app", Sessions: 2}, {Root: "/Users/alex/src/api", Sessions: 1}, {Root: "/Users/alex/src/docs", Sessions: 1}}
	}
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("\n"), &out)
	cfg := config.Config{}
	done, err := offerFirstCapture(p, &cfg, []string{"claude"}, "/Users/alex/src/app", "/Users/alex", known)
	if !done || err != nil {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if !strings.Contains(out.String(), "Your apps also have sessions in 2 other projects. Edit a setting adds them") || !strings.Contains(p.reviewHint, "2 other projects") {
		t.Fatalf("output:\n%s\nhint: %q", &out, p.reviewHint)
	}
	var review bytes.Buffer
	printReviewNotes(&prompter{out: &review, reviewHint: p.reviewHint})
	if !strings.Contains(review.String(), "2 other projects") {
		t.Fatalf("the review does not repeat the hint:\n%s", &review)
	}
}

// Anything but a first setup, such as one that already has apps, asks the
// separate questions with its saved answers as defaults.
func TestOfferFirstCaptureSkipsConfiguredSetups(t *testing.T) {
	t.Parallel()
	for _, cfg := range []config.Config{
		{Harnesses: []string{"claude"}},
		{DeclinedHarnesses: []string{"cursor"}},
	} {
		var out bytes.Buffer
		p := newPrompter(strings.NewReader(""), &out)
		done, err := offerFirstCapture(p, &cfg, []string{"claude"}, "/Users/alex/src/app", "/Users/alex", nil)
		if done || err != nil || out.Len() != 0 {
			t.Fatalf("%+v: done=%v err=%v asked:\n%s", cfg, done, err, &out)
		}
	}
}

// A first setup run from the home folder keeps the separate questions, so
// one Enter never archives everything under it.
func TestSetupFirstRunFromHomeFolderAsksSeparately(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.withApps(t, "claude")
	// The home folder is itself a repository, as with a dotfiles checkout.
	must(t, os.Mkdir(filepath.Join(f.userHome, ".git"), 0o700))
	f.env.WorkingDir = func() (string, error) { return f.userHome, nil }
	project := f.project(t, "src/web-app")
	out := f.runSetup(t, strings.Join([]string{"", project, "", "2", "work", "2", ""}, "\n")+"\n")
	if strings.Contains(out, "Archive Claude Code sessions in") || !strings.Contains(out, "Include Claude Code? [Y/n]") {
		t.Fatalf("home folder was offered on one Enter:\n%s", out)
	}
	cfg, _, err := config.Load(f.home)
	must(t, err)
	for _, saved := range cfg.Archive.Projects {
		if saved.Root == f.userHome {
			t.Fatalf("archived the home folder: %+v", cfg.Archive.Projects)
		}
	}
}

// setupOutput runs setup with the answers and returns what it printed,
// whatever it exited with.
func (f *screenFixture) setupOutput(answers ...string) string {
	var out bytes.Buffer
	Run([]string{"setup"}, strings.NewReader(strings.Join(answers, "\n")+"\n"), &out, &out, f.env)
	return out.String()
}

// A resumed draft that changes apps and projects, and a reconfiguration of
// an installed setup, ask the separate questions with their saved answers,
// never the combined first-run question.
func TestSetupResumeAndReconfigureSkipTheCombinedQuestion(t *testing.T) {
	t.Parallel()
	const combined = "Archive Claude Code sessions in"
	t.Run("resumed draft", func(t *testing.T) {
		t.Parallel()
		f := newScreenFixture(t)
		f.withApps(t, "claude")
		f.inWebApp(t)
		// The first run asks the combined question, then stops at the
		// storage step, leaving a draft.
		if first := f.setupOutput("", "", ""); !strings.Contains(first, combined) {
			t.Fatalf("the first run did not ask the combined question:\n%s", first)
		}
		out := f.setupOutput("capture", "", "", "2", "work", "2", "3")
		if strings.Contains(out, combined) || !strings.Contains(out, "Change which apps are included?") {
			t.Fatalf("resuming did not ask the separate questions:\n%s", out)
		}
	})
	t.Run("reconfigure", func(t *testing.T) {
		t.Parallel()
		f := newScreenFixture(t)
		f.installed(t)
		f.withApps(t, "claude")
		f.inWebApp(t)
		out := f.setupOutput("capture", "n", "n", "")
		if strings.Contains(out, combined) || !strings.Contains(out, "Choose what to capture") || !strings.Contains(out, "Included: Codex.") {
			t.Fatalf("reconfiguring did not ask the separate questions:\n%s", out)
		}
	})
}
