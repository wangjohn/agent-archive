package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/platform"
)

var errClipboardUnavailable = errors.New("no clipboard is available here; choose w to write the handoff to a file")

type clipboardProgram struct {
	name string
	args []string
}

// clipboardCommand selects a provider for the current desktop, never a
// program for another platform or a display that is not configured.
func (e Env) clipboardCommand() (string, []string, error) {
	var programs []clipboardProgram
	switch e.operatingSystem() {
	case platform.Darwin:
		programs = append(programs, clipboardProgram{name: "pbcopy"})
	case platform.Linux:
		if e.getenv("WAYLAND_DISPLAY") != "" {
			programs = append(programs, clipboardProgram{name: "wl-copy", args: []string{"--type", "text/plain;charset=utf-8"}})
		}
		if e.getenv("DISPLAY") != "" {
			programs = append(programs,
				clipboardProgram{name: "xclip", args: []string{"-selection", "clipboard"}},
				clipboardProgram{name: "xsel", args: []string{"--clipboard", "--input"}})
		}
	case platform.Unknown:
		return "", nil, errClipboardUnavailable
	default:
		return "", nil, errClipboardUnavailable
	}
	for _, program := range programs {
		if path, err := e.lookPath(program.name); err == nil {
			return path, program.args, nil
		}
	}
	return "", nil, errClipboardUnavailable
}

func (e Env) clipboardAvailable() bool {
	if e.Clipboard != nil {
		return true
	}
	_, _, err := e.clipboardCommand()
	return err == nil
}

func (e Env) clipboard(data []byte) error {
	if e.Clipboard != nil {
		return e.Clipboard(data)
	}
	program, args, err := e.clipboardCommand()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	// Clipboard providers may fork to keep owning the selection after their
	// parent exits. Do not wait for that child's inherited output pipes.
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Env = e.environ()
	cmd.Stdin = bytes.NewReader(data)
	if out, err := cmd.CombinedOutput(); err != nil {
		if errors.Is(err, exec.ErrWaitDelay) {
			// ErrWaitDelay means the provider itself exited successfully;
			// only its child's pipes remained open.
			return nil
		}
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("clipboard: %w: %s", err, msg)
		}
		return fmt.Errorf("clipboard: %w", err)
	}
	return nil
}
