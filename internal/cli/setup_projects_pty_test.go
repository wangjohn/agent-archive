package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

// This child runs only the selector and review against temporary roots. It
// opens no setup transaction, credentials, scheduler, or provider.
func TestSetupSelectorTerminalChild(t *testing.T) {
	if os.Getenv("ARCHIVE_SETUP_SELECTOR_CHILD") != "1" {
		t.Skip("PTY child only")
	}
	p := newPrompter(os.Stdin, os.Stdout)
	defer p.close()
	if os.Getenv("ARCHIVE_SETUP_YESNO_CHILD") == "1" {
		terminal.Println(p.out, "UNRELATED SENTINEL")
		codex, err := p.setupYesNo("Include Codex?", false)
		must(t, err)
		claude, err := p.setupYesNo("Include Claude Code?", true)
		must(t, err)
		if !codex || claude {
			t.Fatal("resolved setup decisions changed")
		}
		terminal.Println(p.out, "DECISIONS SAVED")
		return
	}
	n, home := 14, ""
	if os.Getenv("ARCHIVE_SETUP_SELECTOR_COMPACT") == "1" {
		n = 2
	}
	roots := selectorRoots(t, n)
	if n == 2 {
		home = filepath.Dir(roots[0].Root)
	}
	terminal.Println(p.out, "UNRELATED SENTINEL")
	got, err := selectSetupProjects(p, nil, nil, roots, "", home, nil)
	must(t, err)
	var names []string
	for _, rule := range got {
		names = append(names, filepath.Base(rule.Root))
	}
	want := 13
	if n == 2 {
		want = 2
	}
	if len(got) != want || includedProjects(got) != want || (n == 14 && got[0].Root == roots[0].Root) {
		t.Fatalf("selection count=%d included=%d roots=%v", len(got), includedProjects(got), names)
	}
	terminal.Println(p.out, "SELECTION ROOTS "+strings.Join(names, ","))
	terminal.Println(p.out, "SELECTION SAVED", want)
}

func TestSetupSelectorTerminalMatrix(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY harness requires Python 3")
	}
	binary, err := os.Executable()
	must(t, err)
	for _, mode := range []string{"80x24", "100x30", "60x20", "36x20", "typed-ahead", "compact", "yes-no"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			out, err := runPTYScript(t, python, setupSelectorPTY, binary, mode)
			if err != nil {
				t.Fatalf("PTY %s: %v\n%s", mode, err, out)
			}
		})
	}
}

const setupSelectorPTY = `import os,pty,select,subprocess,sys,fcntl,termios,struct,time,re,unicodedata
binary,mode=sys.argv[1:]
width,height=(80,24) if mode in ('typed-ahead','compact','yes-no') else tuple(map(int,mode.split('x')))
master,slave=pty.openpty()
fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack('HHHH',height,width,0,0))
env=dict(os.environ,ARCHIVE_SETUP_YESNO_CHILD='1' if mode=='yes-no' else '0',TERM='xterm-256color',NO_COLOR='1',ARCHIVE_SETUP_SELECTOR_CHILD='1',ARCHIVE_SETUP_SELECTOR_COMPACT='1' if mode=='compact' else '0')
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
def cells(data,cols):
 lines=[[' ']*cols];row=col=0;text=data.decode('utf-8','replace');i=0
 def ensure():
  while row>=len(lines): lines.append([' ']*cols)
 while i<len(text):
  if text[i]=='\x1b':
   m=re.match(r'\x1b\[([?0-9;]*)([A-Za-z])',text[i:])
   if m:
    arg,end=m.groups();n=int(arg or '1') if arg.isdigit() or not arg else 1
    if end=='A': row=max(0,row-n)
    elif end=='B': row+=n;ensure()
    elif end=='K':
     if arg=='2': lines[row]=[' ']*cols
     else: lines[row][col:]=[' ']*(cols-col)
    i+=len(m.group());continue
  ch=text[i];i+=1
  if ch=='\r': col=0;continue
  if ch=='\n': row+=1;ensure();continue
  if ch=='\b': col=max(0,col-1);continue
  if ord(ch)<32: continue
  size=0 if unicodedata.combining(ch) else 2 if unicodedata.east_asian_width(ch) in ('W','F') else 1
  if col+size>cols: row+=1;col=0;ensure()
  if size: lines[row][col]=ch;col+=size
 return '\n'.join(''.join(line).rstrip() for line in lines)

try:
 if mode=='yes-no':
  wait(b'Choose [2]');os.write(master,b'y\n')
  wait(b'? Include Claude Code?');os.write(master,b'n\n')
  wait(b'DECISIONS SAVED');p.wait(timeout=30)
  view=cells(out,width)
  assert 'Include Codex Yes' in view and 'Include Claude Code No' in view,view
  assert '? Include' not in view and 'Choose [' not in view,view
  assert 'UNRELATED SENTINEL' in view,view
  assert termios.tcgetattr(slave)[3] & termios.ECHO,'terminal echo not restored'
  assert p.returncode==0,out
  print('setup contextual decisions passed')
  sys.exit(0)
 wait(b'Choose [1]')
 if mode=='compact': os.write(master,b'\n')
 elif mode=='typed-ahead': os.write(master,b'specific\n1\nnext\n\n')
 else:
  os.write(master,b'specific\n');wait(b'Confirm selection');os.write(master,b'1\nnext\n\n')
 count=2 if mode=='compact' else 13
 wait(('SELECTION SAVED %d'%count).encode())
 p.wait(timeout=30)
 while select.select([master],[],[],.05)[0]:
  try:out+=os.read(master,65536)
  except OSError:break
 assert p.returncode==0,out
 view=cells(out,width)
 assert 'UNRELATED SENTINEL' in view,view
 if mode=='compact':
  assert 'Projects All 2 found projects' in view,view
  assert 'Which projects?' not in view and 'Choose [1]' not in view,view
  assert 'project-01,project-02' in view,view
 else:
  assert 'Projects 13 selected' in view,view
  assert b'SELECTION ROOTS '+','.join('project-%02d'%i for i in range(2,15)).encode() in out,out
 assert termios.tcgetattr(slave)[3] & termios.ECHO,'terminal echo not restored'
 print('selector',mode,'passed')
finally:
 if p.poll() is None:
  p.terminate()
  try:p.wait(timeout=5)
  except subprocess.TimeoutExpired:p.kill();p.wait(timeout=5)
 os.close(master);os.close(slave)
`
