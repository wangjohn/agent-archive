package cli

import (
	"os"
	"os/exec"
	"testing"
)

// This child is only launched under a pseudo-terminal by the test below.
func TestSecretTerminalChild(t *testing.T) {
	t.Parallel()
	if os.Getenv("ARCHIVE_SECRET_TEST_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	p := newPrompter(os.Stdin, os.Stdout)
	value, err := p.secret("Secret (hidden): ")
	if err != nil || value != "synthetic-terminal-secret" {
		t.Fatal("hidden input failed")
	}
}

func TestSecretInputDisablesTerminalEcho(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY harness requires Python 3")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := `import os, pty, select, subprocess, sys, termios, time
master, slave = pty.openpty()
env = dict(os.environ, ARCHIVE_SECRET_TEST_CHILD='1')
p = subprocess.Popen([sys.argv[1], '-test.run=^TestSecretTerminalChild$'], stdin=slave, stdout=slave, stderr=slave, env=env)
try:
    deadline = time.monotonic() + 60
    output = b''
    # A color terminal ends the prompt with the › cursor, NO_COLOR with ": ".
    while b'Secret (hidden)' not in output or not (output.endswith('\u203a '.encode()) or output.endswith(b': ')):
        if time.monotonic() > deadline: raise RuntimeError('prompt timeout')
        if select.select([master], [], [], .1)[0]: output += os.read(master, 4096)
    while termios.tcgetattr(slave)[3] & termios.ECHO:
        if time.monotonic() > deadline: raise RuntimeError('echo was not disabled')
        time.sleep(.01)
    os.write(master, b'synthetic-terminal-secret\n')
    while p.poll() is None:
        if time.monotonic() > deadline: raise RuntimeError('child timeout')
        if select.select([master], [], [], .1)[0]: output += os.read(master, 4096)
    assert p.returncode == 0, output
    assert b'synthetic-terminal-secret' not in output, 'secret echoed'
    assert termios.tcgetattr(slave)[3] & termios.ECHO, 'terminal echo not restored'
finally:
    if p.poll() is None: p.kill(); p.wait()
    os.close(master); os.close(slave)
`
	if out, err := runPTYScript(t, python, script, binary); err != nil {
		t.Fatalf("PTY test: %v %s", err, out)
	}
}

func TestSecretInterruptRestoresTerminalEcho(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY harness requires Python 3")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := `import signal, os, pty, select, subprocess, sys, termios, time
master, slave = pty.openpty()
env = dict(os.environ, ARCHIVE_SECRET_TEST_CHILD='1')
p = subprocess.Popen([sys.argv[1], '-test.run=^TestSecretTerminalChild$'], stdin=slave, stdout=slave, stderr=slave, env=env)
try:
    deadline = time.monotonic() + 60
    output = b''
    # A color terminal ends the prompt with the › cursor, NO_COLOR with ": ".
    while b'Secret (hidden)' not in output or not (output.endswith('\u203a '.encode()) or output.endswith(b': ')):
        if time.monotonic() > deadline: raise RuntimeError('prompt timeout')
        if select.select([master], [], [], .1)[0]: output += os.read(master, 4096)
    while termios.tcgetattr(slave)[3] & termios.ECHO:
        if time.monotonic() > deadline: raise RuntimeError('echo was not disabled')
        time.sleep(.01)
    p.send_signal(signal.SIGINT)
    while p.poll() is None:
        if time.monotonic() > deadline: raise RuntimeError('child timeout')
        if select.select([master], [], [], .1)[0]: output += os.read(master, 4096)
    assert p.returncode == 130, (p.returncode, output)
    assert b'synthetic-terminal-secret' not in output, 'secret echoed'
    assert termios.tcgetattr(slave)[3] & termios.ECHO, 'terminal echo not restored'
finally:
    if p.poll() is None: p.kill(); p.wait()
    os.close(master); os.close(slave)
`
	if out, err := runPTYScript(t, python, script, binary); err != nil {
		t.Fatalf("PTY test: %v %s", err, out)
	}
}

// A secret prompt ends with an error at the end of its input: when its
// terminal goes away, and on Ctrl-D with nothing typed. On macOS, once the
// terminal's other end is closed, every read returns no bytes and no error;
// term.ReadPassword took that for "try again" and spun at full CPU forever
// (a child left behind by a killed PTY test kept spinning long after its
// test run ended), and it ignored Ctrl-D the same way.
func TestSecretInputEndsWithItsInput(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY harness requires Python 3")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The "close" mode catches the spin only on macOS: there a read from a
	// terminal whose other end has closed returns no bytes, where Linux
	// returns EIO, an error any reader stops at.
	for _, mode := range []string{"close", "eof"} {
		if out, err := runPTYScript(t, python, secretEndScript, binary, mode); err != nil {
			t.Errorf("PTY test %s: %v %s", mode, err, out)
		}
	}
}

// secretEndScript waits for the hidden prompt, then ends its input: mode
// "close" closes the terminal's other end, and "eof" types Ctrl-D.
const secretEndScript = `import os, pty, select, subprocess, sys, termios, time
binary, mode = sys.argv[1], sys.argv[2]
master, slave = pty.openpty()
env = dict(os.environ, ARCHIVE_SECRET_TEST_CHILD='1')
p = subprocess.Popen([binary, '-test.run=^TestSecretTerminalChild$'], stdin=slave, stdout=slave, stderr=slave, env=env)
try:
    deadline = time.monotonic() + 60
    output = b''
    while b'Secret (hidden)' not in output or not (output.endswith('\u203a '.encode()) or output.endswith(b': ')):
        if time.monotonic() > deadline: raise RuntimeError('prompt timeout', output[-400:])
        if select.select([master], [], [], .1)[0]: output += os.read(master, 4096)
    while termios.tcgetattr(slave)[3] & termios.ECHO:
        if time.monotonic() > deadline: raise RuntimeError('echo was not disabled')
        time.sleep(.01)
    if mode == 'close':
        # Nothing will ever be typed, and no hangup signal comes: the child
        # is not in the terminal's session.
        os.close(master)
        master = None
    else:
        os.write(master, termios.tcgetattr(slave)[6][termios.VEOF])
    limit = min(deadline, time.monotonic() + 30)
    while p.poll() is None:
        if time.monotonic() > limit: raise RuntimeError('the prompt did not end with its input')
        if master is not None and select.select([master], [], [], .05)[0]: output += os.read(master, 4096)
        elif master is None: time.sleep(.05)
    # The child test fails: it got an error, not a secret.
    assert p.returncode == 1, (p.returncode, output[-400:])
    if master is not None:
        assert termios.tcgetattr(slave)[3] & termios.ECHO, 'terminal echo not restored'
finally:
    if p.poll() is None: p.kill(); p.wait()
    if master is not None: os.close(master)
    os.close(slave)
`
