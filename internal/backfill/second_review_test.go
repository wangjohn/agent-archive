package backfill

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// A larger copy of a session that cannot be imported never displaces a copy
// that can: the good copy is imported and the bad one is the duplicate.
func TestDuplicateKeepsTheImportableCopy(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-24 * time.Hour)
	good := map[string]string{}
	for _, id := range []string{"too-large", "empty", "refused"} {
		good[id] = tr.write(filepath.Join("home", claudeFile("a", id)), claudeTranscript(id, repo, start))
	}
	big := tr.write(filepath.Join("home", claudeFile("b", "too-large")), claudeTranscript("too-large", repo, start))
	if err := os.Truncate(big, archive.MaxRecordBytes+1); err != nil {
		t.Fatal(err)
	}
	summary := `{"type":"summary","summary":"recap","leafUuid":"x","cwd":"` + repo + `"}` + "\n"
	tr.write(filepath.Join("home", claudeFile("b", "empty")), strings.Repeat(summary, 20))
	// No record the adapter recognizes: it refuses the file.
	tr.write(filepath.Join("home", claudeFile("b", "refused")), strings.Repeat(`{"type":"mystery","sessionId":"refused","cwd":"`+repo+`"}`+"\n", 20))

	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	for _, c := range p.Candidates {
		want := SkipDuplicateSession
		if c.TranscriptPath == good[c.NativeSessionID] {
			want = ""
		}
		if c.Skip != want {
			t.Errorf("%s (%s): skip %q, want %q", c.NativeSessionID, c.TranscriptPath, c.Skip, want)
		}
	}
	if got := len(p.Imported()); got != 3 {
		t.Fatalf("imported %d sessions, want 3", got)
	}
}

// A missing worktree under home maps to home by its path; when home itself
// is a configured, included project, that configuration wins.
func TestWorktreeMappedToConfiguredHome(t *testing.T) {
	tr := newTree(t)
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(tr.home, true)}}}
	got := newResolver(tr.env(), cfg, Filters{}).resolve(filepath.Join(tr.home, ".claude", "worktrees", "gone"))
	if want := (resolution{root: tr.home, kind: ProjectKindHome, included: true}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// An existing worktree outside the apps' worktree folders whose git
// directory is gone cannot be mapped to its repository.
func TestExistingWorktreeWithMissingGitDir(t *testing.T) {
	tr := newTree(t)
	wt := tr.mkdir("home/elsewhere/feature")
	tr.write("home/elsewhere/feature/.git", "gitdir: "+filepath.Join(tr.root, "gone", ".git", "worktrees", "feature")+"\n")
	if got := newResolver(tr.env(), config.Config{}, Filters{}).resolve(filepath.Join(wt, "pkg")); got != (resolution{skip: SkipWorktreeUnresolved}) {
		t.Fatalf("got %+v", got)
	}
}

// A blank native ID is an identity problem for that session, not a reason
// to abandon the whole plan.
func TestBlankNativeID(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	const id = "0a9b3c4d-0000-4000-8000-0000000000ab"
	tr.write(filepath.Join("home", codexFile(id)), codexTranscript(" ", " ", repo, fixedNow.Add(-time.Hour)))
	p, err := BuildPlan(t.Context(), tr.env(), refusingBlank{}, config.Config{}, Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Candidates) != 1 || p.Candidates[0].Skip != SkipIdentityMismatch {
		t.Fatalf("candidates %+v", p.Candidates)
	}
}

// refusingBlank fails as the local store does for a blank native ID.
type refusingBlank struct{}

func (refusingBlank) Classify(_, nativeSessionID string) (SkipReason, error) {
	if strings.TrimSpace(nativeSessionID) == "" {
		return "", os.ErrInvalid
	}
	return "", nil
}

// A session retention removed is reported as removed_by_retention, ahead of
// the reasons after it, and --include-removed imports it.
func TestRemovedByRetentionInPlan(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-time.Hour)
	tr.write(filepath.Join("home", claudeFile("s", "removed")), claudeTranscript("removed", repo, start))
	tr.write(filepath.Join("home", claudeFile("s", "removed-home")), claudeTranscript("removed-home", tr.home, start))
	st := states{"removed": SkipRemovedByRetention, "removed-home": SkipRemovedByRetention}
	p := plan(t, tr.env(), st, config.Config{}, Filters{})
	for _, id := range []string{"removed", "removed-home"} {
		if c := candidate(t, p, id); c.Skip != SkipRemovedByRetention {
			t.Errorf("%s: %q", id, c.Skip)
		}
	}
	p = plan(t, tr.env(), st, config.Config{}, Filters{IncludeRemoved: true})
	if c := candidate(t, p, "removed"); c.Skip != "" || c.ProjectRoot != repo {
		t.Errorf("with --include-removed: %+v", c)
	}
	if c := candidate(t, p, "removed-home"); c.Skip != SkipHomeDirectory {
		t.Errorf("with --include-removed, the next reason applies: %q", c.Skip)
	}
}

// A session that would register without any start time is unsafe_format:
// no record carries a timestamp and the file's times are unavailable.
func TestZeroStartIsUnsafe(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	path := tr.write(filepath.Join("home", claudeFile("s", "no-time")), `{"type":"user","uuid":"a","sessionId":"no-time","cwd":"`+repo+`","message":{"role":"user","content":"please check it"}}`+"\n")
	env := tr.env()
	env.FileCreated = func(string) (time.Time, error) { return time.Time{}, os.ErrInvalid }
	env.Lstat = func(p string) (fs.FileInfo, error) {
		info, err := os.Lstat(p)
		if err == nil && p == path {
			return zeroTimeInfo{info}, nil
		}
		return info, err
	}
	if c := candidate(t, plan(t, env, nil, config.Config{}, Filters{}), "no-time"); c.Skip != SkipUnsafeFormat || !c.StartedAt.IsZero() {
		t.Fatalf("%+v", c)
	}
}

type zeroTimeInfo struct{ fs.FileInfo }

func (zeroTimeInfo) ModTime() time.Time { return time.Time{} }

// The header scan reads at most headScanLimit bytes: a working directory
// first recorded beyond it is not found, and the session has no project.
func TestHeaderScanCap(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	line := `{"type":"summary","summary":"` + strings.Repeat("x", headLineLimit/2) + `"}` + "\n"
	lead := strings.Repeat(line, headScanLimit/len(line)+1)
	tr.write(filepath.Join("home", claudeFile("s", "late-cwd")), lead+claudeTranscript("late-cwd", repo, fixedNow.Add(-time.Hour)))
	tr.write(filepath.Join("home", claudeFile("s", "early-cwd")), claudeTranscript("early-cwd", repo, fixedNow.Add(-time.Hour))+lead)
	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	if c := candidate(t, p, "late-cwd"); c.Skip != SkipProjectUnknown {
		t.Errorf("late cwd: %q", c.Skip)
	}
	if c := candidate(t, p, "early-cwd"); c.Skip != "" || c.ProjectRoot != repo {
		t.Errorf("early cwd: %+v", c)
	}
}
