package cli

import (
	"os"
	"os/exec"
	"testing"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

// The child traverses guided handoff/file input and pairing's terminal owner,
// then hidden management input. It opens no installations or providers.
func TestGuidedCommandsTerminalChild(t *testing.T) {
	if os.Getenv("ARCHIVE_GUIDED_COMMAND_CHILD") != "1" {
		t.Skip("PTY child only")
	}
	p := newPrompter(os.Stdin, os.Stdout)
	defer p.close()
	choice, err := chooseDestination(p, nil, "", false, productionAgents.Catalog())
	must(t, err)
	if choice.action != handoffWrite {
		t.Fatal("wrong destination")
	}
	// q is consumed by the same buffer, never a second stdin reader.
	must(t, writeHandoffChoice(p, []byte("synthetic handoff"), handoffTarget{}, t.TempDir(), os.Stdout, Env{}))
	must(t, showPairingCode(p, "aardvark-abandoned-abbreviate-abdomen-abhorrence-abiding", Env{LookupEnv: noEnv, Interrupts: noInterrupts}))
	token, err := p.guidedText(promptModel{Question: "Management credential (hidden)", Secret: true})
	must(t, err)
	if token != "synthetic-secret-canary" {
		t.Fatal("hidden input changed")
	}
	terminal.Println(p.out, "DONE")
}

func TestGuidedCommandsTerminalOwnership(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY requires Python")
	}
	binary, err := os.Executable()
	must(t, err)
	for _, mode := range []string{"normal", "term"} {
		t.Run(mode, func(t *testing.T) {
			if out, err := runPTYScript(t, python, guidedCommandsPTY, binary, mode); err != nil {
				t.Fatalf("guided command PTY %s: %v\n%s", mode, err, out)
			}
		})
	}
}

const guidedCommandsPTY = `import os, pty, select, signal, struct, subprocess, sys, termios, time, fcntl
master, slave = pty.openpty()
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH',24,80,0,0))
before=termios.tcgetattr(slave)
env=dict(os.environ,TERM='xterm-256color',ARCHIVE_GUIDED_COMMAND_CHILD='1')
p=subprocess.Popen([sys.argv[1],'-test.run=^TestGuidedCommandsTerminalChild$'],stdin=slave,stdout=slave,stderr=slave,env=env)
output=b''
deadline=time.monotonic()+60
def wait_for(text):
 global output
 while text not in output:
  if (p.poll() is not None and text!=b'<<drained>>') or time.monotonic()>deadline: raise RuntimeError('waiting for',text,output)
  if select.select([master],[],[],.05)[0]:
   try: output+=os.read(master,65536)
   except OSError: pass
try:
 wait_for(b'Choose [print]')
 os.write(master,b'w\nq\n')
 wait_for(b'Press Enter to hide.')
 os.write(master,b'\n')
 wait_for(b'Management credential (hidden)')
 if sys.argv[2]=='term': p.send_signal(signal.SIGTERM)
 else: os.write(master,b'synthetic-secret-canary\n')
 while p.poll() is None:
  if time.monotonic()>deadline: raise RuntimeError('child exit',output)
  if select.select([master],[],[],.05)[0]:
   try: output+=os.read(master,65536)
   except OSError: pass
 os.write(slave,b'<<drained>>')
 wait_for(b'<<drained>>')
 assert p.returncode==(143 if sys.argv[2]=='term' else 0),(p.returncode,output)
 assert b'synthetic-secret-canary' not in output,output
 assert output.index(b'\x1b[?1049h')<output.index(b'Pairing code:')<output.index(b'\x1b[?1049l'),output
 assert output.count(b'aardvark abandoned abbreviate abdomen abhorrence abiding')==1,output
 after=termios.tcgetattr(slave)
 assert before==after,('terminal not restored',before,after,output)
 if sys.argv[2]=='normal': assert b'Credential received' in output and b'DONE' in output,output
finally:
 if p.poll() is None: p.kill();p.wait()
 os.close(master);os.close(slave)
`
