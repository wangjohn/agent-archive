package launchd

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// prototypePlist is the prototype's upload job as it installed it.
const prototypePlist = `<?xml version="1.0"?><plist><dict><key>Label</key><string>com.agent-skills.skill-runs-upload</string><key>ProgramArguments</key><array><string>/usr/bin/python3</string><string>/private/runtime/skill_runs.py</string><string>--home</string><string>/private/records</string><string>upload</string></array></dict></plist>`

func writePlist(t *testing.T, site scheduler.Site, label string, data []byte) {
	t.Helper()
	path := PlistPath(site, scheduler.Ref(label))
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, data, 0o600))
}

func writeCollector(t *testing.T, site scheduler.Site, label, dataHome string) {
	t.Helper()
	data, err := LaunchAgent("/opt/old/agent-archive", dataHome, label, nil)
	must(t, err)
	writePlist(t, site, label, data)
}

func installedRefs(t *testing.T, site scheduler.Site, inst scheduler.Installation) ([]scheduler.Job, error) {
	t.Helper()
	return Scheduler{}.Installed(context.Background(), site, inst)
}

// Its own job comes first, and only jobs of its own data directory follow:
// every collector plist under a label this tool can produce, which runs the
// collector for this directory, in the order the folder lists them. Another
// directory's plist, a plist with no data directory, a plist that is not a
// collector's label, and one that cannot be read are never returned.
func TestInstalledListsEarlierLabelsOfThisDataDirectory(t *testing.T) {
	t.Parallel()
	site := scheduler.Site{UserHome: t.TempDir()}
	data := filepath.Join(t.TempDir(), "data")
	must(t, os.Mkdir(data, 0o700))
	inst := scheduler.Installation{DataHome: data}
	own := CollectorLabel(data, "")
	earlierA, earlierB := LaunchLabel+".aaaaaaaaaaaa", LaunchLabel+".bbbbbbbbbbbb"
	writeCollector(t, site, earlierB, data)
	writeCollector(t, site, earlierA, data)
	writeCollector(t, site, LaunchLabel, data)                                                                                                                           // the label every installation had before labels were per directory
	writeCollector(t, site, LaunchLabel+".cccccccccccc", filepath.Join(data, "elsewhere"))                                                                               // another directory's
	writeCollector(t, site, "com.example.other", data)                                                                                                                   // not this tool's label
	writeCollector(t, site, LaunchLabel+".ABCDEF123456", data)                                                                                                           // not lowercase hex
	writeCollector(t, site, LaunchLabel+".abc", data)                                                                                                                    // not 12 digits
	writePlist(t, site, LaunchLabel+".dddddddddddd", []byte("<plist><dict></dict></plist>"))                                                                             // no data directory
	writePlist(t, site, LaunchLabel+".eeeeeeeeeeee", []byte("<plist><dict><key>EnvironmentVariables</key><dict><key>AGENT_ARCHIVE_HOME</key><string>"+data+"</string>")) // cut off: cannot be read
	must(t, os.Mkdir(PlistPath(site, LaunchLabel+".ffffffffffff"), 0o700))                                                                                               // unreadable
	writeCollector(t, site, own, data)                                                                                                                                   // the installation's own is never an alias
	jobs, err := installedRefs(t, site, inst)
	if err != nil {
		t.Fatal(err)
	}
	// The folder lists file names in order: the default label's plist sorts
	// after the labels that end in letters before "plist".
	want := []scheduler.Job{{Ref: scheduler.Ref(own)}, {Ref: scheduler.Ref(earlierA), Alias: scheduler.EarlierLabel}, {Ref: scheduler.Ref(earlierB), Alias: scheduler.EarlierLabel}, {Ref: LaunchLabel, Alias: scheduler.EarlierLabel}}
	if !slices.Equal(jobs, want) {
		t.Errorf("Installed = %+v\nwant %+v", jobs, want)
	}
	// The default installation's own label is LaunchLabel, so that plist is
	// its own and not an earlier one.
	def := scheduler.Installation{DataHome: data, Default: true}
	jobs, err = installedRefs(t, site, def)
	if err != nil || len(jobs) == 0 || jobs[0] != (scheduler.Job{Ref: LaunchLabel}) || slices.ContainsFunc(jobs[1:], func(j scheduler.Job) bool { return j.Ref == LaunchLabel }) {
		t.Errorf("Installed for the default installation = %+v, %v", jobs, err)
	}
}

// A directory spelled another way is the same directory: a symlink, or the
// same folder by another path, matches (local.SameLocation), which is how a
// collector an earlier release labeled from another spelling is found.
func TestInstalledMatchesADataDirectoryByLocation(t *testing.T) {
	t.Parallel()
	site := scheduler.Site{UserHome: t.TempDir()}
	target := filepath.Join(t.TempDir(), "real")
	must(t, os.Mkdir(target, 0o700))
	link := filepath.Join(t.TempDir(), "link")
	must(t, os.Symlink(target, link))
	earlier := LaunchLabel + ".aaaaaaaaaaaa"
	writeCollector(t, site, earlier, link)
	jobs, err := installedRefs(t, site, scheduler.Installation{DataHome: target})
	if err != nil || len(jobs) != 2 || jobs[1] != (scheduler.Job{Ref: scheduler.Ref(earlier), Alias: scheduler.EarlierLabel}) {
		t.Errorf("Installed = %+v, %v; want the collector under the symlinked spelling", jobs, err)
	}
}

// The prototype's upload job is the account's: only the default installation
// lists it, only when its plist is the prototype's own (its exact label and
// command shape), and one that is not blocks setup with an error that names
// the file. Nothing is executed or changed, and the jobs that can be
// recognized still come back.
func TestInstalledListsThePrototypeOnlyForTheDefaultInstallation(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	def, other := scheduler.Installation{DataHome: data, Default: true}, scheduler.Installation{DataHome: data}
	ownRef := scheduler.Job{Ref: LaunchLabel}
	for _, tc := range []struct {
		name    string
		plist   string // "" for none
		blocked string // the error's words, "" for none
		listed  bool
	}{
		{"no prototype", "", "", false},
		{"the prototype's own plist", prototypePlist, "", true},
		{"another script", strings.Replace(prototypePlist, "skill_runs.py", "unrelated.py", 1), "unrecognized command; preserve", false},
		{"another label", strings.Replace(prototypePlist, "com.agent-skills.skill-runs-upload", "com.example.other", 1), "unrecognized command", false},
		{"another verb", strings.Replace(prototypePlist, "<string>upload</string>", "<string>delete</string>", 1), "unrecognized command", false},
		{"no home argument", strings.Replace(prototypePlist, "<string>/private/records</string>", "<string></string>", 1), "unrecognized command", false},
		{"an extra argument", strings.Replace(prototypePlist, "<string>upload</string>", "<string>upload</string><string>--all</string>", 1), "unrecognized command", false},
		{"unbalanced tags", "<plist><dict><key>Label</key><string>x</dict>", "XML syntax error", false},
		{"cut off", "<plist><dict><key>Label</key>", "cannot verify legacy upload job ownership", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			site := scheduler.Site{UserHome: t.TempDir()}
			if tc.plist != "" {
				writePlist(t, site, LegacyLaunchLabel, []byte(tc.plist))
			}
			jobs, err := installedRefs(t, site, def)
			want := []scheduler.Job{ownRef}
			if tc.listed {
				want = append(want, scheduler.Job{Ref: LegacyLaunchLabel, Alias: scheduler.Prototype})
			}
			if !slices.Equal(jobs, want) {
				t.Errorf("Installed = %+v, want %+v", jobs, want)
			}
			if tc.blocked == "" && err != nil || tc.blocked != "" && (err == nil || !strings.Contains(err.Error(), tc.blocked) || tc.blocked == "unrecognized command; preserve" && !strings.Contains(err.Error(), PlistPath(site, LegacyLaunchLabel))) {
				t.Errorf("Installed error = %v, want one saying %q", err, tc.blocked)
			}
			// Another data directory's installation never sees the prototype.
			jobs, err = installedRefs(t, site, other)
			if err != nil || slices.ContainsFunc(jobs, func(j scheduler.Job) bool { return j.Alias == scheduler.Prototype }) {
				t.Errorf("a non-default installation lists %+v (%v)", jobs, err)
			}
			if tc.plist != "" {
				if data, _ := os.ReadFile(PlistPath(site, LegacyLaunchLabel)); string(data) != tc.plist {
					t.Error("the prototype's plist changed")
				}
			}
		})
	}
	// A prototype plist that cannot be read blocks too, with the read's error.
	site := scheduler.Site{UserHome: t.TempDir()}
	must(t, os.MkdirAll(PlistPath(site, LegacyLaunchLabel), 0o700))
	if _, err := installedRefs(t, site, def); err == nil {
		t.Error("a prototype plist that is a directory blocked nothing")
	}
}

// The prototype's job, when launchd runs it from another plist, comes with
// the fix that goes with it and not the collector's.
func TestPrototypeProblemHasItsOwnFix(t *testing.T) {
	t.Parallel()
	site := scheduler.Site{UserHome: t.TempDir()}
	r := &recorder{answer: func(context.Context, ...string) ([]byte, error) {
		return []byte("\tpath = /home/other/Library/LaunchAgents/" + LegacyLaunchLabel + ".plist\n"), nil
	}}
	got := Scheduler{Run: r.run}.Inspect(context.Background(), site, LegacyLaunchLabel)
	if got.State != scheduler.AnotherInstallation || got.Problem == nil || got.Problem.Fix != "preserve it and resolve it before setup" || got.Problem.Expected != PlistPath(site, LegacyLaunchLabel) {
		t.Errorf("Inspect = %q with %+v", got.State, got.Problem)
	}
}
