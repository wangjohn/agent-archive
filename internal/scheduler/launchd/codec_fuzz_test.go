package launchd

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// xmlText is what the plist codec gives back for a string it wrote as a
// value: XML has no spelling for a character outside its Char production, so
// the writer replaces each with U+FFFD (invalid UTF-8 included), and the
// readers trim surrounding space.
func xmlText(s string) string {
	var b strings.Builder
	for _, r := range s {
		valid := r == '\t' || r == '\n' || r == '\r' ||
			(r >= 0x20 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD && r != utf8.RuneError) || r >= 0x10000
		if !valid {
			r = utf8.RuneError
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// Whatever LaunchAgent is given, the three readers give back what it wrote
// (up to xmlText): the program, the environment with the data directory, and
// the data directory alone, so status and setup --refresh agree with the
// plist setup wrote. Anything LaunchAgent refuses is not checked.
func FuzzLaunchAgentRoundTrip(f *testing.F) {
	f.Add("/usr/local/bin/agent-archive", "/Users/a/.local/share/agent-archive", "com.agent-archive.collector", "PATH", "/usr/bin:/bin")
	f.Add("/Users/someone/Tools & Bin/agent-archive <v2>", "/tmp/a & b", "com.agent-archive.collector.134b03ccc4cc", "AWS_CONFIG_FILE", "/Users/x/AWS & co/config")
	f.Add("/bin/x", "/d", "l", "K", "line one\nline\ttwo\r\n]]></string>")
	f.Add("/ leading and trailing ", "/d", "l", " AGENT_ARCHIVE_HOME ", "v")
	f.Add("/bin/x\x00\x01", "/d\xff", "l", "\x0b", "￾\U0001f600")
	f.Fuzz(func(t *testing.T, executable, dataHome, label, name, value string) {
		executable, dataHome = "/"+executable, "/"+dataHome
		plist, err := LaunchAgent(executable, dataHome, label, map[string]string{name: value})
		if err != nil {
			if name != "" && name != "AGENT_ARCHIVE_HOME" {
				t.Fatalf("LaunchAgent refused %q, %q, %q: %v", executable, dataHome, name, err)
			}
			return
		}
		// executable starts with "/", so it never reads back as empty.
		if program, err := LaunchAgentProgram(plist); err != nil || program != xmlText(executable) {
			t.Fatalf("program %q (err %v), want %q", program, err, xmlText(executable))
		}
		environment, err := LaunchAgentEnvironment(plist)
		if err != nil {
			t.Fatal(err)
		}
		// AGENT_ARCHIVE_HOME is written first and the first of a name wins,
		// so a variable that reads as it (or as nothing) is never seen.
		want := map[string]string{"AGENT_ARCHIVE_HOME": xmlText(dataHome)}
		if key := xmlText(name); key != "" && key != "AGENT_ARCHIVE_HOME" {
			want[key] = xmlText(value)
		}
		if len(environment) != len(want) {
			t.Fatalf("environment %q, want %q", environment, want)
		}
		for key, wantValue := range want {
			if got, ok := environment[key]; !ok || got != wantValue {
				t.Fatalf("%q = %q (%v), want %q", key, got, ok, wantValue)
			}
		}
		if home, err := LaunchAgentDataHome(plist); err != nil || home != want["AGENT_ARCHIVE_HOME"] {
			t.Fatalf("data home %q (err %v), want %q", home, err, want["AGENT_ARCHIVE_HOME"])
		}
	})
}

// The readers take any bytes, as status reads plists a person may have edited
// or another program wrote: they return or fail, never panic, and the data
// directory is what the environment says.
func FuzzLaunchAgentReaders(f *testing.F) {
	f.Add([]byte(`<plist><dict><key>ProgramArguments</key><array><string>/bin/x</string></array><key>EnvironmentVariables</key><dict><key>AGENT_ARCHIVE_HOME</key><string>/d</string></dict></dict></plist>`))
	f.Add([]byte(`<plist><dict><key>EnvironmentVariables</key><dict><key>A</key><dict><key>B</key><string>x</string></dict></dict></dict>`))
	f.Add([]byte(`{"ProgramArguments": ["/bin/true"]}`))
	f.Add([]byte(``))
	f.Fuzz(func(t *testing.T, plist []byte) {
		_, _ = LaunchAgentProgram(plist)
		environment, err := LaunchAgentEnvironment(plist)
		home, homeErr := LaunchAgentDataHome(plist)
		if (err == nil) != (homeErr == nil) {
			t.Fatalf("environment error %v, data home error %v", err, homeErr)
		}
		if err == nil && (environment == nil || home != environment["AGENT_ARCHIVE_HOME"]) {
			t.Fatalf("data home %q, environment %q", home, environment)
		}
	})
}
