package cli

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
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
    deadline = time.monotonic() + 10
    output = b''
    while b'Secret (hidden): ' not in output:
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", script, binary)
	if out, err := cmd.CombinedOutput(); err != nil {
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
    deadline = time.monotonic() + 10
    output = b''
    while b'Secret (hidden): ' not in output:
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", script, binary)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("PTY test: %v %s", err, out)
	}
}
