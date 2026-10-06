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
	host := e.CodexLabelHost
	if host == nil {
		host = e.startCodexLabelHost
	}
	return e.agentRegistry().WithCodexLabelHost(host)
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

func (e Env) startCodexLabelHost(ctx context.Context, home string) (agentapi.LabelTransport, error) {
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
	h := &codexLabelHost{cmd: cmd, input: input, output: output, lines: make(chan labelLine, 1), done: make(chan struct{}), waited: make(chan struct{})}
	cmd.Stderr = &labelDiscard{limit: 16 << 10, exceeded: func() { _ = cmd.Process.Kill() }}
	if cmd.Start() != nil {
		_ = input.Close()
		_ = output.Close()
		return nil, agentapi.ErrLabelHostUnavailable
	}
	go h.readLines()
	go func() { _ = cmd.Wait(); close(h.waited) }()
	return h, nil
}

type labelLine struct {
	data []byte
	err  error
}

type codexLabelHost struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	output io.ReadCloser
	lines  chan labelLine
	done   chan struct{}
	waited chan struct{}
	once   sync.Once
	write  sync.Mutex
}

func (h *codexLabelHost) readLines() {
	defer close(h.lines)
	reader := bufio.NewReaderSize(io.LimitReader(h.output, (1<<20)+1), (256<<10)+1)
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
	h.once.Do(func() { close(h.done); _ = h.input.Close(); _ = h.output.Close(); _ = h.cmd.Process.Kill() })
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-h.waited:
		return nil
	case <-timer.C:
		return agentapi.ErrLabelTerminationUnavailable
	}
}

type labelDiscard struct {
	limit    int
	exceeded func()
}

func (w *labelDiscard) Write(p []byte) (int, error) {
	w.limit -= len(p)
	if w.limit < 0 {
		w.exceeded()
		return len(p), agentapi.ErrLabelBudgetExceeded
	}
	return len(p), nil
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
