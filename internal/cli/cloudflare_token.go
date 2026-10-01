package cli

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

const maxManagementTokenBytes = 4096

type tokenOutput struct {
	data     []byte
	overflow bool
}

func (b *tokenOutput) Write(p []byte) (int, error) {
	n := len(p)
	available := maxManagementTokenBytes - len(b.data)
	if n > available {
		b.overflow = true
		p = p[:available]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func managementChildEnvironment(env Env) []string {
	var result []string
	for _, entry := range env.environ() {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "ACCESS_KEY") || strings.HasPrefix(upper, "AGENT_ARCHIVE_PAIR") || strings.HasPrefix(upper, "AWS_") {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func (env Env) runManagementTokenCommand(ctx context.Context, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	environment := managementChildEnvironment(env)
	if env.RunTokenCommand != nil {
		return env.RunTokenCommand(ctx, append([]string(nil), args...), environment)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = environment
	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 100 * time.Millisecond
	var output tokenOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", errors.New("token command failed; check the program's exit status and unlock it interactively")
		}
		return "", errors.New("token command could not complete")
	}
	if output.overflow {
		return "", errors.New("token command output exceeded its limit")
	}
	return string(output.data), nil
}

func readManagementToken(ctx context.Context, p *prompter, env Env, command []string, interactive bool) (token string, fromEnv, removed bool, err error) {
	if value, ok := env.lookupEnv("CLOUDFLARE_API_TOKEN"); ok {
		if env.unsetEnv("CLOUDFLARE_API_TOKEN") != nil {
			return "", true, false, errors.New("could not remove CLOUDFLARE_API_TOKEN; no management operation was started")
		}
		value = strings.TrimSpace(value)
		if value != "" {
			return value, true, true, validateManagementToken(value)
		}
	}
	if len(command) > 0 {
		if err := (config.Config{CloudflareTokenCommand: command}).ValidateCloudflareTokenCommand(); err != nil {
			return "", false, false, err
		}
		if !interactive {
			return "", false, false, errors.New("cloudflare_token_command requires an explicit interactive invocation; set CLOUDFLARE_API_TOKEN for --yes")
		}
		value, e := env.runManagementTokenCommand(ctx, command)
		if e != nil {
			return "", false, false, errors.New("management token command failed")
		}
		value = strings.TrimSpace(value)
		return value, false, false, validateManagementToken(value)
	}
	if !interactive || p == nil {
		return "", false, false, errors.New("set CLOUDFLARE_API_TOKEN for this explicit provider operation")
	}
	value, e := p.secret("Cloudflare API token (hidden; Enter to choose another option): ")
	if e != nil {
		return "", false, false, errors.New("could not read management token")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false, false, errChooseStorageAgain
	}
	return value, false, false, validateManagementToken(value)
}

func validateManagementToken(value string) error {
	if len(value) == 0 || len(value) > maxManagementTokenBytes || strings.ContainsAny(value, "\r\n\x00\t ") {
		return errors.New("management token is empty or malformed")
	}
	return nil
}
