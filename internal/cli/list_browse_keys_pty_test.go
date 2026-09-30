//go:build darwin || linux

package cli

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// This child is only launched under a pseudo-terminal by the test below:
// the browser reading keys from a real terminal.
func TestBrowserKeysTerminalChild(t *testing.T) {
	t.Parallel()
	if os.Getenv("ARCHIVE_KEYS_TEST_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	env, _, _ := publishedFixture(t)
	env.openKeys = terminalKeys
	// The real terminal, its size, and signals.
	env.IsTerminal, env.TerminalSize, env.Interrupts = nil, nil, nil
	if code := Run([]string{"list"}, os.Stdin, os.Stdout, os.Stderr, env); code != 0 {
		t.Fatalf("code %d", code)
	}
}

// On a real terminal the browser turns off echo and line editing while it
// reads keys, and turns them back on however it ends: quitting, Ctrl-C,
// or Ctrl-Z (until the shell continues it).
func TestBrowserKeysRestoreTheTerminal(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY harness requires Python 3")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"quit", "interrupt", "suspend"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, python, "-c", keysPTYScript, binary, mode)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("PTY test: %v %s", err, out)
			}
		})
	}
}

const keysPTYScript = `import fcntl, os, pty, select, signal, struct, subprocess, sys, termios, time
binary, mode = sys.argv[1], sys.argv[2]
master, slave = pty.openpty()
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 24, 100, 0, 0))
env = dict(os.environ, ARCHIVE_KEYS_TEST_CHILD='1', TERM='xterm-256color')
# As a shell does: this script leads a session whose controlling terminal
# is the pty, and runs the child as the foreground job, so Ctrl-C and
# Ctrl-Z typed on the pty signal it, and the pty outlives it.
os.setsid()
fcntl.ioctl(slave, termios.TIOCSCTTY, 0)
def foreground():
    os.setpgid(0, 0)
    signal.signal(signal.SIGTTOU, signal.SIG_IGN)
    os.tcsetpgrp(0, os.getpid())
    signal.signal(signal.SIGTTOU, signal.SIG_DFL)
p = subprocess.Popen([binary, '-test.run=^TestBrowserKeysTerminalChild$'], stdin=slave, stdout=slave, stderr=slave, env=env, preexec_fn=foreground)
deadline = time.monotonic() + 20
output = b''
def pump():
    global output
    if time.monotonic() > deadline: raise RuntimeError('timeout: %r' % output[-400:])
    if select.select([master], [], [], .05)[0]:
        try: output += os.read(master, 65536)
        except OSError: pass
def keys_on():
    lflag = termios.tcgetattr(slave)[3]
    return not (lflag & termios.ECHO) and not (lflag & termios.ICANON) and lflag & termios.ISIG
def wait_for(text):
    while text not in output: pump()
def wait_until(check, what):
    while not check():
        if time.monotonic() > deadline: raise RuntimeError(what)
        pump()
def finish(code):
    while p.poll() is None: pump()
    assert p.returncode == code, (p.returncode, output[-800:])
try:
    wait_for(b'or q to quit')
    wait_until(keys_on, 'key mode not on at the list')
    if mode == 'quit':
        os.write(master, b'\x1b[B\x1b[A1\r')
        wait_for(b't transcript')
        assert keys_on(), 'key mode off at the details'
        os.write(master, b'q')
        finish(0)
    elif mode == 'interrupt':
        os.write(master, b'\x03')
        finish(130)
    else:
        os.write(master, b'\x1a')
        _, status = os.waitpid(p.pid, os.WUNTRACED)
        assert os.WIFSTOPPED(status), status
        assert termios.tcgetattr(slave)[3] & termios.ECHO, 'echo off while stopped'
        p.send_signal(signal.SIGCONT)
        wait_until(keys_on, 'key mode not back on after continuing')
        os.write(master, b'q')
        finish(0)
    lflag = termios.tcgetattr(slave)[3]
    assert lflag & termios.ECHO and lflag & termios.ICANON, 'terminal modes not restored'
    assert b'\x1b[?1049l' in output, 'alternate screen not left'
finally:
    if p.poll() is None: p.kill(); p.wait()
    # Closing the pty hangs up its session, this script.
    signal.signal(signal.SIGHUP, signal.SIG_IGN)
    os.close(master); os.close(slave)
`
