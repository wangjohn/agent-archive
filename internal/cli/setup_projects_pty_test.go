package cli

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
)

// This child runs only the selector and review against temporary roots. It
// opens no setup transaction, credentials, scheduler, or provider.
func TestSetupSelectorTerminalChild(t *testing.T) {
	if os.Getenv("ARCHIVE_SETUP_SELECTOR_CHILD") != "1" {
		t.Skip("PTY child only")
	}
	p := newPrompter(os.Stdin, os.Stdout)
	defer p.close()
	roots := selectorRoots(t, 14)
	fmt.Fprintln(p.out, "UNRELATED SENTINEL")
	got, err := selectSetupProjects(p, nil, nil, roots, "", "", nil)
	must(t, err)
	if len(got) != 14 || includedProjects(got) != 13 {
		t.Fatalf("selection %+v", got)
	}
	fmt.Fprintln(p.out, "SELECTION SAVED 13")
}

func TestSetupSelectorTerminalMatrix(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY harness requires Python 3")
	}
	binary, err := os.Executable()
	must(t, err)
	for _, mode := range []string{"80x24", "100x30", "60x20", "36x20", "typed-ahead"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			out, err := runPTYScript(t, python, setupSelectorPTY, binary, mode)
			if err != nil {
				t.Fatalf("PTY %s: %v\n%s", mode, err, out)
			}
		})
	}
}

const setupSelectorPTY = `import os,pty,select,subprocess,sys,fcntl,termios,struct,time,re
binary,mode=sys.argv[1:]
width,height=(80,24) if mode=='typed-ahead' else tuple(map(int,mode.split('x')))
master,slave=pty.openpty()
fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack('HHHH',height,width,0,0))
env=dict(os.environ,TERM='xterm-256color',NO_COLOR='1',ARCHIVE_SETUP_SELECTOR_CHILD='1')
p=subprocess.Popen([binary,'-test.run=^TestSetupSelectorTerminalChild$'],stdin=slave,stdout=slave,stderr=slave,env=env)
out=b''
def wait(text):
 global out
 limit=time.monotonic()+30
 while text not in out:
  if p.poll() is not None or time.monotonic()>limit: raise RuntimeError('waiting for',text,out[-2000:])
  if select.select([master],[],[],.05)[0]:
   try:out+=os.read(master,65536)
   except OSError:pass
wait(b'Choose [1]')
if mode=='typed-ahead': os.write(master,b'specific\n1\nnext\n\n')
else:
 os.write(master,b'specific\n');wait(b'Confirm selection');os.write(master,b'1\nnext\n\n')
wait(b'SELECTION SAVED 13')
p.wait(timeout=30)
while select.select([master],[],[],.05)[0]:
 try:out+=os.read(master,65536)
 except OSError:break
assert p.returncode==0,out
assert b'UNRELATED SENTINEL' in out,out
assert b'synthetic-secret' not in out,out
assert b'Projects 13 selected' in out,out
print('selector',mode,'passed')
`
