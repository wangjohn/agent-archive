package cli

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

type promptEmissionMode string

const (
	emissionComplete    promptEmissionMode = "complete"
	emissionPartial     promptEmissionMode = "partial"
	emissionPartialLong promptEmissionMode = "partial-long"
	emissionBefore      promptEmissionMode = "before"
	emissionEOF         promptEmissionMode = "eof"
	emissionPanic       promptEmissionMode = "panic"
	emissionWriteError  promptEmissionMode = "write-error"
	emissionSignal      promptEmissionMode = "signal"
	emissionSuspend     promptEmissionMode = "suspend"
)

// splitPromptOutput waits for a separate harness pipe after writing the
// question prefix. It never reads stdin: the prompter remains its sole reader.
type splitPromptOutput struct {
	ack   *os.File
	mode  promptEmissionMode
	split bool
}

func (*splitPromptOutput) promptCapabilities() promptCapabilities {
	return capabilitiesFor(os.Stdin, os.Stdout)
}

func (o *splitPromptOutput) Write(b []byte) (int, error) {
	const prefix = "? AWS profile\n"
	if !o.split {
		if i := strings.Index(string(b), prefix); i >= 0 {
			o.split = true
			split := i + len(prefix)
			n, err := os.Stdout.Write(b[:split])
			if err != nil {
				return n, err
			}
			var ack [1]byte
			if _, err = o.ack.Read(ack[:]); err != nil {
				return n, err
			}
			if o.mode == emissionPanic {
				panic("synthetic prompt write panic")
			}
			if o.mode == emissionWriteError {
				return n, errors.New("synthetic prompt write error")
			}
			more, err := os.Stdout.Write(b[split:])
			return n + more, err
		}
	}
	return os.Stdout.Write(b)
}

func TestGuidedPromptEmissionTerminalChild(t *testing.T) {
	if os.Getenv("ARCHIVE_PROMPT_EMISSION_CHILD") != "1" {
		t.Skip("PTY child only")
	}
	fd, err := strconv.Atoi(os.Getenv("ARCHIVE_PROMPT_ACK_FD"))
	must(t, err)
	ack := os.NewFile(uintptr(fd), "prompt-write-handshake")
	defer func() { _ = ack.Close() }()
	mode := promptEmissionMode(os.Getenv("ARCHIVE_PROMPT_EMISSION_MODE"))
	out := &splitPromptOutput{ack: ack, mode: mode}
	p := newPrompter(os.Stdin, out)
	defer p.close()
	terminal.Println(p.out, "UNRELATED SENTINEL")
	if mode == emissionBefore {
		terminal.Println(p.out, "BEFORE INPUT")
		var ready [1]byte
		_, err = ack.Read(ready[:])
		must(t, err)
	}
	value, err := p.guidedText(promptModel{Question: "AWS profile", Label: "Profile", Receipt: "Profile", Default: "work"})
	must(t, err)
	want := "work"
	if mode == emissionPartialLong || mode == emissionBefore {
		want = strings.Repeat("x", 170)
	}
	if value != want {
		t.Fatal("input value changed")
	}
	terminal.Println(p.out, "DONE")
}

func TestGuidedPromptEmissionPreservesTerminalOwnership(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY harness requires Python 3")
	}
	binary, err := os.Executable()
	must(t, err)
	for _, mode := range []promptEmissionMode{emissionComplete, emissionPartial, emissionPartialLong, emissionBefore, emissionEOF, emissionPanic, emissionWriteError, emissionSignal, emissionSuspend} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			if out, err := runPTYScript(t, python, promptEmissionPTY, binary, string(mode)); err != nil {
				t.Fatalf("PTY %s: %v\n%s", mode, err, out)
			}
		})
	}
}

const promptEmissionPTY = `import os,pty,select,subprocess,sys,termios,fcntl,struct,time,signal,re,unicodedata
binary,mode=sys.argv[1:]
master,slave=pty.openpty()
fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack('HHHH',24,80,0,0))
ackRead,ackWrite=os.pipe()
env=dict(os.environ,TERM='xterm-256color',NO_COLOR='1',ARCHIVE_PROMPT_EMISSION_CHILD='1',ARCHIVE_PROMPT_ACK_FD=str(ackRead),ARCHIVE_PROMPT_EMISSION_MODE=mode)
p=subprocess.Popen([binary,'-test.run=^TestGuidedPromptEmissionTerminalChild$'],stdin=slave,stdout=slave,stderr=slave,env=env,pass_fds=(ackRead,))
os.close(ackRead)
output=b''
def read():
 global output
 if select.select([master],[],[],.05)[0]:
  try:output+=os.read(master,65536)
  except OSError:pass
def wait(needle):
 limit=time.monotonic()+30
 while needle not in output:
  if (p.poll() is not None and needle!=b'<<drained>>') or time.monotonic()>limit:raise RuntimeError('waiting for',needle,output[-2000:])
  read()
def echo(on):
 limit=time.monotonic()+30
 while bool(termios.tcgetattr(slave)[3]&termios.ECHO)!=on:
  if p.poll() is not None or time.monotonic()>limit:raise RuntimeError('echo mode',on,output[-2000:])
  read()
def cells(data,cols):
    lines=[[' ']*cols];row=col=0; text=data.decode('utf-8','replace');i=0
    def ensure():
        while row>=len(lines): lines.append([' ']*cols)
    while i<len(text):
        if text[i]=='\x1b':
            m=re.match(r'\x1b\[([?0-9;]*)([A-Za-z])',text[i:])
            if m:
                arg,end=m.groups(); n=int(arg or '1') if arg.isdigit() or not arg else 1
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
 if mode=='before':
  wait(b'BEFORE INPUT');os.write(master,b'x'*170);os.write(ackWrite,b'a')
 wait(b'? AWS profile')
 if mode=='signal': p.send_signal(signal.SIGTERM)
 else:
  if mode=='suspend':
   p.send_signal(signal.SIGTSTP)
   limit=time.monotonic()+30
   while True:
    pid,status=os.waitpid(p.pid,os.WNOHANG|os.WUNTRACED)
    if pid and os.WIFSTOPPED(status):break
    if time.monotonic()>limit:raise RuntimeError('no actual stop')
    read()
   assert termios.tcgetattr(slave)[3]&termios.ECHO,'suspended emission did not restore echo'
   os.write(slave,b'\r\nSHELL SENTINEL\r\n')
   p.send_signal(signal.SIGCONT)
  if mode=='complete':os.write(master,b'work\n')
  elif mode=='partial':os.write(master,b'wo')
  elif mode=='partial-long':os.write(master,b'x'*170)
  elif mode=='eof':os.write(master,termios.tcgetattr(slave)[6][termios.VEOF])
  os.write(ackWrite,b'b')
  if mode not in ('complete','eof','panic'):
   if mode!='write-error':wait(b'Profile [work]: ')
   echo(True)
   if mode=='partial':os.write(master,b'rk\n')
   elif mode in ('partial-long','before'):os.write(master,b'\n')
   else:os.write(master,b'work\n')
 limit=time.monotonic()+30
 while p.poll() is None:
  if time.monotonic()>limit:raise RuntimeError('child exit',output[-2000:])
  read()
 os.write(slave,b'<<drained>>');wait(b'<<drained>>')
 assert termios.tcgetattr(slave)[3]&termios.ECHO,'emission did not restore echo'
 expected=143 if mode=='signal' else 2 if mode=='panic' else 1 if mode=='eof' else 0
 assert p.returncode==expected,(p.returncode,expected,output)
 view=cells(output,80)
 assert 'UNRELATED SENTINEL' in view,view
 if expected==0:
  assert 'DONE' in view and ('✓ Profile' if mode in ('partial-long','before') else 'Profile work') in view,view
  if mode in ('partial-long','before'):assert b'x'*170 in output,'long input value lost'
  if mode in ('complete','partial-long','before','write-error','suspend'):
   assert b'\x1b[2K' not in output,'erased a region with unproven echo: '+repr(output)
  if mode in ('partial-long','before'):assert 'AWS profile' in view,view
  if mode=='partial':assert 'AWS profile' not in view,view
  if mode=='suspend':assert 'SHELL SENTINEL' in view,view
finally:
 if p.poll() is None:p.kill();p.wait()
 os.close(master);os.close(slave);os.close(ackWrite)
`
