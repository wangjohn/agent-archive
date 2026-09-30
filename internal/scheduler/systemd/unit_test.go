package systemd

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/scheduler"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// One word of a value is written so that systemd reads back exactly it: bare
// when nothing in it is read as anything else, else in double quotes, with
// the backslash, the quote and the specifier's percent sign escaped, and the
// dollar sign too in a command's arguments. splitWords is the reader those
// rules are written for (it is systemd's own word splitting, in the subset a
// unit file holds).
func TestWordQuoting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value     string
		arguments bool
		want      string
	}{
		{"/usr/bin/agent-archive", true, "/usr/bin/agent-archive"},
		{"", false, `""`},
		{"My Apps", false, `"My Apps"`},
		{"100%", false, "100%%"},
		{"$HOME/x", true, `"$$HOME/x"`},
		{"$HOME/x", false, `"$HOME/x"`},
		{`say "hi" \ it's`, false, `"say \"hi\" \\ it's"`},
		{"a;b", true, `"a;b"`},
		{"trailing\\", false, `"trailing\\"`},
		{" padded ", false, `" padded "`},
		{"UTF-8: é", false, `"UTF-8: é"`},
	} {
		got := word(tc.value, tc.arguments)
		if got != tc.want {
			t.Errorf("word(%q, %v) = %s, want %s", tc.value, tc.arguments, got, tc.want)
		}
		// A `$` doubled for ExecStart= is expanded by systemd, not by this reader.
		if back, err := splitWords(got); !tc.arguments && (err != nil || !slices.Equal(back, []string{tc.value})) {
			t.Errorf("splitWords(%s) = %q (%v), want %q", got, back, err, tc.value)
		}
	}
}

// The reader is systemd's word splitting: quotes group, backslash escapes
// resolve, `%%` is one percent sign and no other specifier is touched.
func TestSplitWords(t *testing.T) {
	t.Parallel()
	for value, want := range map[string][]string{
		`a b  c`:                    {"a", "b", "c"},
		`"a b" 'c d' e"f g"h`:       {"a b", "c d", "ef gh"},
		`"q\"r" '\\' "l1\nl2\tX"`:   {`q"r`, `\`, "l1\nl2\tX"},
		`x\sy`:                      {"x y"},
		`100%% %h %%h`:              {"100%", "%h", "%h"},
		`PATH="/a b:/c" HOME=/root`: {"PATH=/a b:/c", "HOME=/root"},
		`""`:                        {""},
		``:                          nil,
	} {
		if got, err := splitWords(value); err != nil || !slices.Equal(got, want) {
			t.Errorf("splitWords(%q) = %q (%v), want %q", value, got, err, want)
		}
	}
	for _, bad := range []string{`"open`, `'open`, `trailing\`, `not\ known`} {
		if got, err := splitWords(bad); err == nil {
			t.Errorf("splitWords(%q) = %q, want an error", bad, got)
		}
	}
}

// A unit file another program or a person wrote is read as systemd reads it:
// comments, sections, continued lines, a setting given twice (the later wins;
// an empty one clears), and a program with a prefix or quotes.
func TestReadServiceReadsAnyUnit(t *testing.T) {
	t.Parallel()
	program, env, err := readService([]byte(`# comment
[Unit]
Environment=NOT=service

[Service]
; another comment
ExecStart=
ExecStart=-"/opt/my apps/agent-archive" _collect
Environment=A=1 B="two words" \
    C=3
Environment=B=later
Environment=D=x
Environment=
Environment=E=%% AGENT_ARCHIVE_HOME=/data
`))
	if err != nil || program != "/opt/my apps/agent-archive" {
		t.Fatalf("program %q (%v)", program, err)
	}
	if want := map[string]string{"E": "%", "AGENT_ARCHIVE_HOME": "/data"}; !maps.Equal(env, want) {
		t.Errorf("environment %q, want %q", env, want)
	}
	// A continued line goes on past comment lines, which are dropped rather
	// than joined, and a backslash that is itself escaped continues nothing.
	program, env, err = readService([]byte(`[Service]
ExecStart=\
  # a comment, even one ending in a backslash \
  ; and another
  /bin/x _collect
Environment=A=1 \
# B=2
  C=3
Environment=D=\\
Environment=E=5
`))
	if want := map[string]string{"A": "1", "C": "3", "D": `\`, "E": "5"}; err != nil || program != "/bin/x" || !maps.Equal(env, want) {
		t.Errorf("continued lines read as program %q, environment %q (%v); want /bin/x, %q", program, env, err, want)
	}
	// An empty line ends a continued line: this ExecStart= is empty.
	for _, bad := range []string{"[Service]\nExecStart=\\\n\n/bin/x\n", "", "not a unit", "[Service]\nExecStart=\n", "[Service]\nExecStart=\"open\n", "[Unit]\nExecStart=/bin/x\n", "[Service]\nExecStart=/bin/x\nEnvironment=\"open\n"} {
		if program, env, err := readService([]byte(bad)); err == nil {
			t.Errorf("readService(%q) = %q, %q; want an error", bad, program, env)
		}
	}
}

// Plan refuses what a unit file cannot hold, or would read as something else,
// rather than write it.
func TestPlanRefusesWhatAUnitCannotHold(t *testing.T) {
	t.Parallel()
	spec := func(change func(*scheduler.JobSpec)) scheduler.JobSpec {
		s := scheduler.JobSpec{Executable: "/usr/bin/agent-archive", Args: []string{"_collect"}, DataHome: "/data", Env: map[string]string{"A": "b"}, Interval: time.Minute, RunAtLoad: true}
		change(&s)
		return s
	}
	inst, site := scheduler.Installation{DataHome: "/data"}, scheduler.Site{UserHome: "/home/u"}
	if _, err := (Scheduler{}).Plan(site, inst, spec(func(*scheduler.JobSpec) {})); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*scheduler.JobSpec){
		"another command":     func(s *scheduler.JobSpec) { s.Args = []string{"_hook"} },
		"another interval":    func(s *scheduler.JobSpec) { s.Interval = time.Hour },
		"not run at load":     func(s *scheduler.JobSpec) { s.RunAtLoad = false },
		"relative executable": func(s *scheduler.JobSpec) { s.Executable = "agent-archive" },
		"relative data home":  func(s *scheduler.JobSpec) { s.DataHome = "data" },
		"dollar in program":   func(s *scheduler.JobSpec) { s.Executable = "/opt/$bin/agent-archive" },
		// systemd refuses these in a program's path, quoted or not.
		"an apostrophe in program": func(s *scheduler.JobSpec) { s.Executable = "/home/o'brien/bin/agent-archive" },
		"a quote in program":       func(s *scheduler.JobSpec) { s.Executable = `/opt/"x"/agent-archive` },
		"a backslash in program":   func(s *scheduler.JobSpec) { s.Executable = `/opt/x\y/agent-archive` },
		// and these from v261.
		"a star in program":     func(s *scheduler.JobSpec) { s.Executable = "/opt/*/agent-archive" },
		"a question in program": func(s *scheduler.JobSpec) { s.Executable = "/opt/x?/agent-archive" },
		"a bracket in program":  func(s *scheduler.JobSpec) { s.Executable = "/opt/[x]/agent-archive" },
		"a newline in a path":   func(s *scheduler.JobSpec) { s.DataHome = "/da\nta" },
		"a newline in a value": func(s *scheduler.JobSpec) {
			s.Env = map[string]string{"A": "x\nExecStart=/bin/evil"}
		},
		"invalid UTF-8":       func(s *scheduler.JobSpec) { s.Env = map[string]string{"A": "\xff"} },
		"a DEL in a value":    func(s *scheduler.JobSpec) { s.Env = map[string]string{"A": "x\x7f"} },
		"a non-character":     func(s *scheduler.JobSpec) { s.Env = map[string]string{"A": "x￾"} },
		"a name with a space": func(s *scheduler.JobSpec) { s.Env = map[string]string{"A B": "x"} },
		"an empty name":       func(s *scheduler.JobSpec) { s.Env = map[string]string{"": "x"} },
		"a digit first":       func(s *scheduler.JobSpec) { s.Env = map[string]string{"1A": "x"} },
		"the data directory":  func(s *scheduler.JobSpec) { s.Env = map[string]string{"AGENT_ARCHIVE_HOME": "/x"} },
	} {
		if plan, err := (Scheduler{}).Plan(site, inst, spec(change)); err == nil {
			t.Errorf("Plan accepted %s: %+v", name, plan)
		}
	}
	// A home with an apostrophe keeps its data directory there, and a program
	// systemd cannot run is refused with what to do about it.
	if _, err := (Scheduler{}).Plan(site, inst, spec(func(s *scheduler.JobSpec) { s.DataHome = "/home/o'brien/.local/share/agent-archive" })); err != nil {
		t.Errorf("Plan refused a data directory with an apostrophe: %v", err)
	}
	for _, exe := range []string{"/home/o'brien/.local/bin/agent-archive", "/opt/$x/agent-archive"} {
		_, err := (Scheduler{}).Plan(site, inst, spec(func(s *scheduler.JobSpec) { s.Executable = exe }))
		if err == nil || !strings.Contains(err.Error(), "AGENT_ARCHIVE_INSTALL_DIR") {
			t.Errorf("Plan(%s) = %v, want a refusal that says where to install instead", exe, err)
		}
	}
}
