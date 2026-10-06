package cli

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

// TestGuidedSetupTerminalChild uses setup's exact provider model with synthetic
// answers. No setup transaction, home, credential store or provider is opened.
func TestGuidedSetupTerminalChild(t *testing.T) {
	if os.Getenv("ARCHIVE_GUIDED_PROMPT_CHILD") != "1" {
		t.Skip("PTY child only")
	}
	p := newPrompter(os.Stdin, os.Stdout)
	defer p.close()
	mode := os.Getenv("ARCHIVE_GUIDED_PROMPT_MODE")
	model := promptExample()
	model.Helpers = nil
	if mode == "external" || mode == "pager" {
		r := p.renderer()
		region := r.begin(model)
		release := p.suspendPrompts()
		terminal.Println(p.out, "\nEXTERNAL SENTINEL")
		if mode == "pager" {
			terminal.Print(p.out, "\x1b[?1049hPAGER CONTENT\x1b[?1049l")
		}
		raw, interrupted, err := p.guidedRead(nil)
		must(t, err)
		release()
		r.finish(region, "Provider Amazon S3", raw, false, p.inputPending(), interrupted)
	} else {
		answer, err := p.guidedChoice(model)
		must(t, err)
		if answer != "s3" {
			t.Fatalf("provider %s", answer)
		}
	}
	profileDefault := "work"
	if mode == "default-long" {
		profileDefault = strings.Repeat("d", 90)
	}
	value, err := p.guidedText(promptModel{Question: "AWS profile", Label: "Profile", Default: profileDefault, Receipt: "Profile"})
	must(t, err)
	if mode == "long" && value != strings.Repeat("x", 90) {
		t.Fatalf("lost long answer")
	}
	secret, err := p.guidedText(promptModel{Question: "Secret access key (hidden)", Label: "Credential", Secret: true})
	must(t, err)
	if secret != "synthetic-secret" {
		t.Fatalf("lost hidden input")
	}
	terminal.Println(p.out, "DONE")
}

// TestGuidedPromptTerminalCells proves ownership by interpreting terminal cells,
// rather than accepting an ANSI sequence as evidence that collapse was safe.
func TestGuidedPromptTerminalCells(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY harness requires Python 3")
	}
	binary, err := os.Executable()
	must(t, err)
	for _, mode := range []string{"normal", "no-color", "long", "default-long", "retry", "resize", "scroll", "typed-ahead", "suspend", "external", "pager", "eof", "secret-eof", "interrupt", "term", "hup", "quit"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			if out, err := runPTYScript(t, python, guidedPromptPTY, binary, mode); err != nil {
				t.Fatalf("PTY %s: %v\n%s", mode, err, out)
			}
		})
	}
}

const guidedPromptPTY = `import os, pty, select, subprocess, sys, termios, time, fcntl, struct, signal, re, unicodedata
binary, mode = sys.argv[1:]
master, slave = pty.openpty()
width, height = (36,20) if mode == 'scroll' else (60,20) if mode in ('long','default-long') else (100,30) if mode == 'no-color' else (80,24)
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH',height,width,0,0))
env = dict(os.environ, TERM='xterm-256color', ARCHIVE_GUIDED_PROMPT_CHILD='1', ARCHIVE_GUIDED_PROMPT_MODE=mode)
env.pop('NO_COLOR',None)
if mode == 'no-color': env['NO_COLOR']='1'
os.setsid()
fcntl.ioctl(slave, termios.TIOCSCTTY, 0)
def session():
    os.setpgid(0,0)
    signal.signal(signal.SIGTTOU,signal.SIG_IGN)
    os.tcsetpgrp(0,os.getpid())
    signal.signal(signal.SIGTTOU,signal.SIG_DFL)
p = subprocess.Popen([binary,'-test.run=^TestGuidedSetupTerminalChild$'],stdin=slave,stdout=slave,stderr=slave,env=env,preexec_fn=session)
output = b''
deadline = time.monotonic()+60

def read():
    global output
    if select.select([master],[],[],.05)[0]:
        try: output += os.read(master,65536)
        except OSError: pass

def wait(needle,offset=0):
    limit=min(deadline,time.monotonic()+30)
    while needle not in output[offset:]:
        if p.poll() is not None or time.monotonic()>limit: raise RuntimeError('waiting for',needle,output[-1500:])
        read()

def echo(on):
    limit=min(deadline,time.monotonic()+30)
    while bool(termios.tcgetattr(slave)[3] & termios.ECHO)!=on:
        if time.monotonic()>limit: raise RuntimeError('echo mode',on,output[-1500:])
        read()

# This small terminal oracle handles the renderer's cursor/erase vocabulary,
# canonical echo and wrap. Keep scrollback, so erasing an unrelated row fails.
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
    wait(b'Choose [2]: ')
    if mode=='eof': os.write(master,termios.tcgetattr(slave)[6][termios.VEOF])
    else:
        if mode=='retry':
            offset=len(output);os.write(master,b'bad\n');wait(b'Choose [2]: ',offset)
        if mode=='typed-ahead': os.write(master,b'2\nwork\nsynthetic-secret\n')
        else: os.write(master,b'2\n')
        wait(b'Profile ['+ (b'd'*90 if mode=='default-long' else b'work') + b']: ')
        if mode=='resize':
            signal.signal(signal.SIGTTOU,signal.SIG_IGN)
            fcntl.ioctl(master,termios.TIOCSWINSZ,struct.pack('HHHH',20,60,0,0))
            signal.signal(signal.SIGTTOU,signal.SIG_DFL)
            p.send_signal(signal.SIGWINCH)
        if mode!='typed-ahead': os.write(master,(b'x'*90 if mode=='long' else b'x'*900 if mode=='scroll' else b'' if mode=='default-long' else b'work')+b'\n')
        wait(b'Credential: ')
        if mode!='typed-ahead': echo(False)
        if mode=='suspend':
            p.send_signal(signal.SIGTSTP)
            while True:
                pid,status=os.waitpid(p.pid,os.WNOHANG|os.WUNTRACED)
                if pid and os.WIFSTOPPED(status): break
                if time.monotonic()>deadline: raise RuntimeError('did not suspend')
                read()
            assert termios.tcgetattr(slave)[3] & termios.ECHO, 'suspend did not restore echo'
            os.write(slave,b'\r\nSHELL SENTINEL\r\n')
            p.send_signal(signal.SIGCONT);echo(False)
        sig={'interrupt':signal.SIGINT,'term':signal.SIGTERM,'hup':signal.SIGHUP,'quit':signal.SIGQUIT}.get(mode)
        if sig: p.send_signal(sig)
        elif mode=='secret-eof': os.write(master,termios.tcgetattr(slave)[6][termios.VEOF])
        elif mode!='typed-ahead': os.write(master,b'synthetic-secret\n')
    while p.poll() is None:
        if time.monotonic()>deadline: raise RuntimeError('child exit',output[-1500:])
        read()
    marker=b"<<drained>>"
    os.write(slave,marker)
    while marker not in output:
        if time.monotonic()>deadline: raise RuntimeError("drain timeout",output[-1500:])
        read()
    output=output.replace(marker,b"")
    expected={'eof':1,'secret-eof':1,'interrupt':130,'term':143,'hup':129,'quit':131}.get(mode,0)
    assert p.returncode==expected,(p.returncode,expected,output[-1500:])
    assert termios.tcgetattr(slave)[3] & termios.ECHO,'echo not restored'
    if mode!='typed-ahead': assert b'synthetic-secret' not in output,'hidden input echoed'
    assert b'\x1b[?1049' not in output or mode=='pager','setup used alternate screen'
    view=cells(output,width)
    if expected==0:
        assert 'DONE' in view,view
        assert 'Credential received' in view,view
        assert 'Provider Amazon S3' in view,view
        if mode in ('normal','no-color','long','default-long'):
            assert 'Where should your archive live?' not in view,view
            assert 'AWS profile' not in view,view
            assert 'Secret access key' not in view,view
        if mode=='no-color': assert b'\x1b[1m' not in output and b'\x1b[2m' not in output,'NO_COLOR styled text'
        if mode=='resize': assert 'AWS profile' in view,view
        if mode=='scroll': assert 'AWS profile' in view,view
        if mode=='suspend': assert 'SHELL SENTINEL' in view,view
        if mode in ('external','pager'): assert 'EXTERNAL SENTINEL' in view and 'Where should your archive live?' in view,view
finally:
    if p.poll() is None: p.kill();p.wait()
    signal.signal(signal.SIGHUP,signal.SIG_IGN)
    os.close(master);os.close(slave)
`
