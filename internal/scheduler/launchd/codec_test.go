package launchd

import (
	"strings"
	"testing"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// status reads back the program a LaunchAgent runs to notice a moved binary,
// so the reader must agree with the writer, including XML-escaped paths.
func TestLaunchAgentProgramReadsWhatLaunchAgentWrites(t *testing.T) {
	for _, executable := range []string{"/Applications/agent-archive", "/Users/someone/Tools & Bin/agent-archive <v2>"} {
		plist, err := LaunchAgent(executable, "/Users/someone/.local/share/agent-archive", LaunchLabel, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := LaunchAgentProgram(plist)
		if err != nil || got != executable {
			t.Fatalf("read %q (err %v), want %q", got, err, executable)
		}
	}
	for name, plist := range map[string]string{
		"no ProgramArguments": `<plist><dict><key>Label</key><string>x</string><key>Program</key><string>/bin/true</string></dict></plist>`,
		"empty arguments":     `<plist><dict><key>ProgramArguments</key><array></array></dict></plist>`,
		"not a plist":         `{"ProgramArguments": ["/bin/true"]}`,
	} {
		if got, err := LaunchAgentProgram([]byte(plist)); err == nil {
			t.Errorf("%s: read %q, want an error", name, got)
		}
	}
}

func TestLaunchAgentEscapesPaths(t *testing.T) {
	b, e := LaunchAgent("/a & b/agent-archive", "/private/data", LaunchLabel, nil)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(b), "/a &amp; b/agent-archive") {
		t.Fatal("invalid XML")
	}
	if strings.Contains(string(b), "sync") {
		t.Fatal("public sync used for scheduled internal mode")
	}
}

// status compares what the collector runs with to what setup verified, so
// the environment LaunchAgent writes must read back exactly, escaped values
// included, alongside the data directory.
func TestLaunchAgentEnvironmentReadsWhatLaunchAgentWrites(t *testing.T) {
	environment := map[string]string{
		"AWS_CONFIG_FILE": "/Users/someone/AWS & co/config",
		"PATH":            "/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin",
	}
	plist, err := LaunchAgent("/bin/agent-archive", "/private/data", LaunchLabel, environment)
	if err != nil {
		t.Fatal(err)
	}
	got, err := LaunchAgentEnvironment(plist)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"AGENT_ARCHIVE_HOME": "/private/data", "AWS_CONFIG_FILE": environment["AWS_CONFIG_FILE"], "PATH": environment["PATH"]}
	if len(got) != len(want) {
		t.Fatalf("environment %v, want %v", got, want)
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("%s = %q, want %q", name, got[name], value)
		}
	}
	if home, err := LaunchAgentDataHome(plist); err != nil || home != "/private/data" {
		t.Fatalf("data home %q (err %v)", home, err)
	}
	if _, err := LaunchAgent("/bin/agent-archive", "/private/data", LaunchLabel, map[string]string{"AGENT_ARCHIVE_HOME": "/elsewhere"}); err == nil {
		t.Fatal("LaunchAgent let the environment replace the data directory")
	}
}

func TestCollectorLabelKeepsTheDefaultAndSeparatesOthers(t *testing.T) {
	def := "/Users/u/.local/share/agent-archive"
	if got := CollectorLabel(def, def); got != LaunchLabel {
		t.Fatalf("default data directory relabeled to %s", got)
	}
	a, b := CollectorLabel("/tmp/a", def), CollectorLabel("/tmp/b", def)
	if a == LaunchLabel || a == b || !strings.HasPrefix(a, LaunchLabel+".") || a != CollectorLabel("/tmp/a/", def) {
		t.Fatalf("labels %s and %s", a, b)
	}
	plist, err := LaunchAgent("/bin/agent-archive", "/tmp/a & b", a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plist), "<string>"+a+"</string>") {
		t.Fatal("plist does not carry its label")
	}
	if home, err := LaunchAgentDataHome(plist); err != nil || home != "/tmp/a & b" {
		t.Fatalf("data home %q err %v", home, err)
	}
	if home, err := LaunchAgentDataHome([]byte(`<plist><dict><key>Label</key><string>x</string></dict></plist>`)); err != nil || home != "" {
		t.Fatalf("no environment: %q %v", home, err)
	}
}

// status and setup --refresh read plists a person or another program may have
// written, with keys in any order: the readers take the program only from
// ProgramArguments, and the environment only from EnvironmentVariables' own
// strings, not from an array or a dict nested elsewhere.
func TestLaunchAgentReadersTakeOnlyTheirOwnKeys(t *testing.T) {
	plist := []byte(`<plist><dict>
<key>WatchPaths</key><array><string>/not/the/program</string></array>
<key>EnvironmentVariables</key><dict>
<key>NESTED</key><dict><key>INNER</key><string>/not/a/variable</string></dict>
<key>AGENT_ARCHIVE_HOME</key><string>/d</string>
</dict>
<key>ProgramArguments</key><array><string>/bin/agent-archive</string><string>_collect</string></array>
</dict></plist>`)
	if program, err := LaunchAgentProgram(plist); err != nil || program != "/bin/agent-archive" {
		t.Fatalf("program %q (err %v)", program, err)
	}
	environment, err := LaunchAgentEnvironment(plist)
	if err != nil {
		t.Fatal(err)
	}
	if len(environment) != 1 || environment["AGENT_ARCHIVE_HOME"] != "/d" {
		t.Fatalf("environment %q", environment)
	}
}
