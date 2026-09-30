package systemd

import (
	"maps"
	"strings"
	"testing"
)

// Whatever renderService is given, the reader gives back exactly what it
// wrote: the program, the environment, and the data directory, so status and
// setup --refresh agree with the unit setup wrote (there is no trimming or
// replacement, as there is in a plist). Nothing given it adds a setting: every
// line of the unit is one of the settings it writes, so a value cannot inject
// an ExecStart= or a section. Anything renderService refuses is not checked.
func FuzzRenderServiceRoundTrip(f *testing.F) {
	f.Add("/usr/local/bin/agent-archive", "/home/a/.local/share/agent-archive", "PATH", "/usr/local/bin:/usr/bin")
	f.Add("/opt/My Apps/agent-archive", "/tmp/100% $HOME's data", "AWS_PROFILE", `it's "work" \ 50%`)
	f.Add("/bin/x", "/d", "K", "line one\nExecStart=/bin/evil")
	f.Add("/bin/x", "/d", "K", "trailing backslash\\")
	f.Add("/ leading and trailing ", "/d ", "K", "  ")
	f.Add("/bin/x", "/d", "K", "%h %%h %n $$ ${X} ; ' \" \\\" \\n")
	f.Add("/bin/x\x00", "/d\xff", "1A B", "\U0001f600\ufffe")
	f.Add("/bin/x", "/d", "AGENT_ARCHIVE_HOME", "/elsewhere")
	f.Fuzz(func(t *testing.T, executable, dataHome, name, value string) {
		executable, dataHome = "/"+executable, "/"+dataHome
		unit, err := renderService(executable, dataHome, map[string]string{name: value})
		if err != nil {
			if validName(name) && name != "AGENT_ARCHIVE_HOME" && validText(executable) && validText(dataHome) && validText(value) && !strings.Contains(executable, "$") {
				t.Fatalf("renderService refused %q, %q, %q=%q: %v", executable, dataHome, name, value, err)
			}
			return
		}
		program, environment, err := readService(unit)
		if err != nil || program != strings.TrimLeft(executable, "@-:+!") {
			t.Fatalf("program %q (%v), want %q", program, err, executable)
		}
		want := map[string]string{"AGENT_ARCHIVE_HOME": dataHome, name: value}
		if !maps.Equal(environment, want) {
			t.Fatalf("environment %q, want %q", environment, want)
		}
		for line := range strings.SplitSeq(strings.TrimSuffix(string(unit), "\n"), "\n") {
			ok := line == "" || line[0] == '#' || line[0] == '['
			for _, setting := range []string{"Description=", "Type=", "ExecStart=", "Environment=", "StandardOutput=append:", "StandardError=append:"} {
				ok = ok || strings.HasPrefix(line, setting)
			}
			if !ok {
				t.Fatalf("the unit has the line %q, which no setting of this package writes:\n%s", line, unit)
			}
		}
	})
}

// The readers take any bytes, as status reads unit files a person may have
// edited or another program wrote: they return or fail, never panic, and
// splitting words never returns a word with an unbalanced quote unread.
func FuzzReadService(f *testing.F) {
	f.Add([]byte("[Service]\nExecStart=/bin/x\nEnvironment=A=1 \"B=2 3\"\n"))
	f.Add([]byte("[Service]\nExecStart=/bin/x \\\n"))
	f.Add([]byte("[Service]\nEnvironment=\"A=\\\n"))
	f.Add([]byte("[Service]\r\nExecStart=\"\"\r\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, unit []byte) {
		program, environment, err := readService(unit)
		if err == nil && (program == "" || environment == nil) {
			t.Fatalf("read %q as program %q, environment %v with no error", unit, program, environment)
		}
	})
}
