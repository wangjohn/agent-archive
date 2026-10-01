package systemd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// enable makes the link `systemctl --user enable --now <ref>.timer` makes: in
// timers.target.wants, pointing at the timer's file by its absolute path.
func enable(t *testing.T, s Scheduler, site scheduler.Site, ref scheduler.Ref) string {
	t.Helper()
	link := filepath.Join(s.UnitDir(site), "timers.target.wants", string(ref)+".timer")
	must(t, os.MkdirAll(filepath.Dir(link), 0o755))
	must(t, os.Symlink(s.timerPath(site, ref), link))
	return link
}

func noManager() Scheduler {
	return Scheduler{Run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("Definition asked the manager")
	}}
}

// The enable link is one of the files of a job uninstall removes, so a job
// whose unit files were deleted without a `disable` (uninstall
// --skip-scheduler) leaves no dangling link behind: it is in Paths while the
// unit files are there, and after they are gone, after the two unit files.
func TestDefinitionListsTheEnableLinkAfterTheUnitFiles(t *testing.T) {
	t.Parallel()
	s := noManager()
	site := scheduler.Site{UserHome: t.TempDir()}
	ref := write(t, s, site)
	if got := s.Definition(site, ref).Paths; len(got) != 2 {
		t.Fatalf("a job that was never enabled has paths %q, want its two unit files", got)
	}
	link := enable(t, s, site, ref)
	want := []string{s.servicePath(site, ref), s.timerPath(site, ref), link}
	if got := s.Definition(site, ref).Paths; !slices.Equal(got, want) {
		t.Errorf("paths %q, want %q", got, want)
	}
	if got := s.Inspect(context.Background(), site, ref).Paths; len(got) != 3 || got[2] != link {
		t.Errorf("Inspect's paths %q lack the enable link", got)
	}

	// The unit files are deleted and the link dangles: the job is not defined
	// any more, and the link is still listed so it can be removed.
	must(t, os.Remove(s.servicePath(site, ref)))
	must(t, os.Remove(s.timerPath(site, ref)))
	got := s.Definition(site, ref)
	if got.Defined || !slices.Equal(got.Paths, want) {
		t.Errorf("after the unit files are gone: defined %v, paths %q, want undefined and %q", got.Defined, got.Paths, want)
	}
}

// Only a link that is this job's own is listed. One at that path that is not
// a link, or points at another installation's timer or at another unit, or
// at a timer under another home, is left alone by uninstall: its name is
// ours, but it is not ours to delete.
func TestDefinitionNeverListsAnotherJobsEnableLink(t *testing.T) {
	t.Parallel()
	s := noManager()
	site := scheduler.Site{UserHome: t.TempDir()}
	ref := write(t, s, site)
	linkDir := filepath.Join(s.UnitDir(site), "timers.target.wants")
	link := filepath.Join(linkDir, string(ref)+".timer")
	must(t, os.MkdirAll(linkDir, 0o755))
	other := scheduler.Ref("agent-archive-collector-0123456789ab")
	elsewhere := scheduler.Site{UserHome: t.TempDir()}

	for _, tc := range []struct {
		name string
		make func()
	}{
		{"a regular file", func() { must(t, os.WriteFile(link, []byte("not a link"), 0o600)) }},
		{"a directory", func() { must(t, os.Mkdir(link, 0o755)) }},
		{"a link to another installation's timer", func() { must(t, os.Symlink(s.timerPath(site, other), link)) }},
		{"a link to the service", func() { must(t, os.Symlink(s.servicePath(site, ref), link)) }},
		{"a link to a timer of another home", func() { must(t, os.Symlink(s.timerPath(elsewhere, ref), link)) }},
		{"a link to something that is not a unit", func() { must(t, os.Symlink("/dev/null", link)) }},
		{"a link to nothing", func() { must(t, os.Symlink(filepath.Join(site.UserHome, "gone"), link)) }},
	} {
		must(t, os.RemoveAll(link))
		tc.make()
		if got := s.Definition(site, ref).Paths; slices.Contains(got, link) || len(got) != 2 {
			t.Errorf("%s: paths %q list a link that is not this job's", tc.name, got)
		}
	}

	// Its own, spelled relatively, is its own.
	must(t, os.RemoveAll(link))
	must(t, os.Symlink(filepath.Join("..", string(ref)+".timer"), link))
	if got := s.Definition(site, ref).Paths; !slices.Contains(got, link) {
		t.Errorf("a relative link to the job's own timer is not listed: %q", got)
	}
}

// Another installation's job has a link of its own under another name, and
// this job's paths never include it.
func TestDefinitionListsOnlyItsOwnJobsLink(t *testing.T) {
	t.Parallel()
	s := noManager()
	site := scheduler.Site{UserHome: t.TempDir()}
	ref := write(t, s, site)
	other := scheduler.Ref("agent-archive-collector-0123456789ab")
	otherLink := filepath.Join(s.UnitDir(site), "timers.target.wants", string(other)+".timer")
	must(t, os.MkdirAll(filepath.Dir(otherLink), 0o755))
	must(t, os.Symlink(s.timerPath(site, other), otherLink))
	if got := s.Definition(site, ref).Paths; slices.Contains(got, otherLink) || len(got) != 2 {
		t.Errorf("paths %q include another installation's link", got)
	}
	if got := s.Definition(site, other).Paths; !slices.Contains(got, otherLink) {
		t.Errorf("the other installation's own paths %q lack its link", got)
	}
}

// A home reached through a link is one location under either spelling: the
// manager may write the link's target under the home's real path while setup
// names the home by the link (or the other way round), and the link is the
// job's own either way.
func TestDefinitionListsItsOwnLinkUnderEitherSpellingOfTheHome(t *testing.T) {
	t.Parallel()
	s := noManager()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	spelled := filepath.Join(t.TempDir(), "home")
	must(t, os.Symlink(resolved, spelled))
	for _, tc := range []struct {
		name   string
		home   string
		target string
	}{
		{"a target under the real path", spelled, resolved},
		{"a target under the linked path", resolved, spelled},
	} {
		site := scheduler.Site{UserHome: tc.home}
		ref := write(t, s, site)
		link := filepath.Join(s.UnitDir(site), "timers.target.wants", string(ref)+".timer")
		must(t, os.MkdirAll(filepath.Dir(link), 0o755))
		must(t, os.RemoveAll(link))
		must(t, os.Symlink(s.timerPath(scheduler.Site{UserHome: tc.target}, ref), link))
		if got := s.Definition(site, ref).Paths; !slices.Contains(got, link) {
			t.Errorf("%s: paths %q lack the job's own link", tc.name, got)
		}
	}
}
