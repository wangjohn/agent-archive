package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

func TestSetupDetailsPagerTerminalChild(t *testing.T) {
	if os.Getenv("ARCHIVE_SETUP_DETAILS_PAGER_CHILD") != "1" {
		t.Skip("PTY child only")
	}
	p := newPrompter(os.Stdin, os.Stdout)
	defer p.close()
	p.reviewModel = buildSetupReviewModel(config.Config{}, setupReview{}, time.Now())
	env := Env{LookupEnv: func(name string) (string, bool) { return "test-pager", name == "PAGER" }, Interrupts: noInterrupts}
	env.RunPager = func(_ context.Context, _ string, _ []string, in io.Reader, out, _ io.Writer) error {
		if _, err := io.WriteString(out, "PAGER SELECTED\n"); err != nil {
			return err
		}
		_, err := io.Copy(out, in)
		return err
	}
	must(t, showSetupReviewDetails(p, env, os.Stderr))
}

func TestSetupDetailsPagerUsesDefaultTerminalDetection(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY harness requires Python 3")
	}
	binary, err := os.Executable()
	must(t, err)
	if out, err := runPTYScript(t, python, setupDetailsPagerPTY, binary); err != nil {
		t.Fatalf("Details pager terminal: %v\n%s", err, out)
	}
}

const setupDetailsPagerPTY = `import os,pty,select,subprocess,sys,time
master,slave=pty.openpty()
p=None
output=b''
try:
    env=dict(os.environ,ARCHIVE_SETUP_DETAILS_PAGER_CHILD='1')
    p=subprocess.Popen([sys.argv[1],'-test.run=^TestSetupDetailsPagerTerminalChild$'],stdin=slave,stdout=slave,stderr=slave,env=env)
    deadline=time.monotonic()+30
    while p.poll() is None:
        if time.monotonic()>deadline: raise RuntimeError('Details pager timed out',output)
        if select.select([master],[],[],.05)[0]:
            output+=os.read(master,65536)
    while select.select([master],[],[],.05)[0]:
        output+=os.read(master,65536)
    if p.returncode!=0 or b'PAGER SELECTED' not in output or b'Full settings and privacy' not in output:
        raise RuntimeError('Details did not use pager',p.returncode,output)
finally:
    if p is not None:
        if p.poll() is None: p.kill()
        p.wait()
    os.close(slave)
    os.close(master)
`
