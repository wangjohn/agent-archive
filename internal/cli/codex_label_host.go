package cli

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
)

func (e Env) labelProviders(cfg config.Config) agentapi.LabelsLookup {
	if cfg.CodexNameLookup != config.CodexNameLookupNative {
		return e.agentRegistry()
	}
	return e.agentRegistry().WithCodexLabelHost(e.codexLabelHostFactory())
}

// runPass composes a fresh factory for one synchronous collector pass.
func (e Env) codexLabelHostFactory() agentapi.LabelHostFactory {
	host := e.CodexLabelHost
	if host == nil {
		budget := &labelStreamBudget{remaining: 1 << 20, stderrRemaining: 16 << 10}
		host = func(ctx context.Context, home string) (agentapi.LabelTransport, error) {
			return e.startCodexLabelHost(ctx, home, budget)
		}
	}
	return host
}

// Carry native user settings and authentication through HOME/config, without
// forwarding archive credentials, AWS variables or shell authentication keys.
func (e Env) codexLabelEnvironment(home string) []string {
	out := []string{"CODEX_HOME=" + home, "CODEX_SQLITE_HOME=" + home}
	for _, key := range []string{"HOME", "PATH", "TMPDIR", "TMP", "TEMP", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "LANG", "LC_ALL"} {
		value := e.getenv(key)
		if key == "HOME" {
			if actual, err := e.userHomeDir(); err == nil {
				value = actual
			}
		}
		if value != "" {
			out = append(out, key+"="+value)
		}
	}
	return out
}

func (e Env) startCodexLabelHost(ctx context.Context, home string, budget *labelStreamBudget) (agentapi.LabelTransport, error) {
	if budget == nil {
		budget = &labelStreamBudget{remaining: 1 << 20, stderrRemaining: 16 << 10}
	}
	path, err := e.labelExecutable(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, agentapi.ErrLabelHostMissing
	}
	// No shell, login, model or turn command. The native host can still mutate
	// its own database/config cache; explicit opt-in is required by composition.
	cmd := exec.CommandContext(ctx, path, "app-server")
	cmd.Env = e.codexLabelEnvironment(home)
	cmd.Dir = home
	cmd.WaitDelay = 250 * time.Millisecond
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, agentapi.ErrLabelHostUnavailable
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		_ = input.Close()
		return nil, agentapi.ErrLabelHostUnavailable
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = input.Close()
		_ = output.Close()
		return nil, agentapi.ErrLabelHostUnavailable
	}
	h := &codexLabelHost{cmd: cmd, input: input, output: output, stderr: stderr, streamBudget: budget, lines: make(chan labelLine, 1), done: make(chan struct{}), waited: make(chan struct{}), stdoutDone: make(chan struct{}), stderrDone: make(chan struct{})}
	if cmd.Start() != nil {
		_ = input.Close()
		_ = output.Close()
		_ = stderr.Close()
		return nil, agentapi.ErrLabelHostUnavailable
	}
	go h.readLines()
	go h.readStderr()
	go func() { _ = cmd.Wait(); close(h.waited) }()
	return h, nil
}

type labelLine struct {
	data []byte
	err  error
}

type codexLabelHost struct {
	cmd          *exec.Cmd
	input        io.WriteCloser
	output       io.ReadCloser
	stderr       io.ReadCloser
	streamBudget *labelStreamBudget
	lines        chan labelLine
	done         chan struct{}
	waited       chan struct{}
	stdoutDone   chan struct{}
	stderrDone   chan struct{}
	once         sync.Once
	write        sync.Mutex
}

func (h *codexLabelHost) readLines() {
	if h.stdoutDone != nil {
		defer close(h.stdoutDone)
	}
	defer close(h.lines)
	reader := bufio.NewReaderSize(io.LimitReader(labelBudgetReader{reader: h.output, budget: h.streamBudget}, (1<<20)+1), (256<<10)+1)
	for {
		line, err := reader.ReadSlice('\n')
		if len(line) > 256<<10 {
			err = agentapi.ErrLabelBudgetExceeded
			line = nil
		} else if err != nil && len(line) > 0 {
			err = agentapi.ErrLabelProtocolUnavailable
			line = nil
		}
		if err == nil {
			line = append([]byte(nil), line[:len(line)-1]...)
		}
		select {
		case h.lines <- labelLine{line, err}:
		case <-h.done:
			return
		}
		if err != nil {
			return
		}
	}
}

// Reserve before pipe reads so concurrent read-ahead never exceeds the pass cap.
// Small reservations keep idle hosts from holding a full line's allowance.
type labelStreamBudget struct {
	mu              sync.Mutex
	remaining       int
	stderrRemaining int
}

type labelBudgetReader struct {
	reader io.Reader
	budget *labelStreamBudget
	stderr bool
}

func (r labelBudgetReader) Read(p []byte) (int, error) {
	r.budget.mu.Lock()
	remaining := &r.budget.remaining
	chunkLimit := 4096
	if r.stderr {
		remaining = &r.budget.stderrRemaining
		chunkLimit = 256
	}
	allowance := min(len(p), *remaining, chunkLimit)
	*remaining -= allowance
	r.budget.mu.Unlock()
	if allowance == 0 {
		return 0, agentapi.ErrLabelBudgetExceeded
	}
	n, err := r.reader.Read(p[:allowance])
	r.budget.mu.Lock()
	*remaining += allowance - n
	r.budget.mu.Unlock()
	return n, err
}

func (h *codexLabelHost) ReadLine(ctx context.Context) ([]byte, error) {
	select {
	case line, ok := <-h.lines:
		if !ok {
			return nil, io.EOF
		}
		return line.data, line.err
	case <-ctx.Done():
		_ = h.Close()
		return nil, ctx.Err()
	case <-h.done:
		return nil, agentapi.ErrLabelHostClosed
	}
}

func (h *codexLabelHost) WriteLine(ctx context.Context, line []byte) error {
	if len(line) > 4096 || strings.ContainsAny(string(line), "\r\n") {
		return agentapi.ErrLabelRequestRejected
	}
	result := make(chan error, 1)
	go func() {
		h.write.Lock()
		defer h.write.Unlock()
		_, err := h.input.Write(append(append([]byte(nil), line...), '\n'))
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = h.Close()
		return ctx.Err()
	case <-h.done:
		return agentapi.ErrLabelHostClosed
	}
}

func (h *codexLabelHost) Close() error {
	h.once.Do(func() {
		close(h.done)
		_ = h.input.Close()
		_ = h.output.Close()
		_ = h.stderr.Close()
		_ = h.cmd.Process.Kill()
	})
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-h.waited:
	case <-timer.C:
		return agentapi.ErrLabelTerminationUnavailable
	}
	for _, done := range []chan struct{}{h.stdoutDone, h.stderrDone} {
		select {
		case <-done:
		case <-timer.C:
			return agentapi.ErrLabelTerminationUnavailable
		}
	}
	return nil
}

// Discard only bytes reserved from the shared pass allowance, before pipe reads.
func discardLabelStderr(reader io.Reader, budget *labelStreamBudget) error {
	_, err := io.Copy(io.Discard, labelBudgetReader{reader: reader, budget: budget, stderr: true})
	return err
}

func (h *codexLabelHost) readStderr() {
	defer close(h.stderrDone)
	if discardLabelStderr(h.stderr, h.streamBudget) != nil {
		_ = h.cmd.Process.Kill()
	}
}

func (e Env) labelEnvironment(cfg config.Config, homes []string) agentapi.LabelEnvironment {
	mode := agentapi.LabelLookupFiles
	if cfg.CodexNameLookup == config.CodexNameLookupNative {
		mode = agentapi.LabelLookupNative
	}
	contract := ""
	if provider, ok := e.labelProviders(cfg).LookupLabels("codex"); ok {
		if builder, ok := provider.(agentapi.LabelContextProvider); ok {
			contract = builder.LabelContextVersion()
		}
	}
	return agentapi.LabelEnvironment{Mode: mode, ProviderContract: contract, Homes: homes, ExternalSQLite: e.getenv("CODEX_SQLITE_HOME") != ""}
}

func (e Env) labelExecutable(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	type found struct {
		path string
		err  error
	}
	result := make(chan found, 1)
	go func() { path, err := e.lookPath("codex"); result <- found{path, err} }()
	select {
	case value := <-result:
		return value.path, value.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
