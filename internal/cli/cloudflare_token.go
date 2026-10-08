package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
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
		value, e := runPromptManagementTokenCommand(ctx, p, env, command)
		value = strings.TrimSpace(value)
		if e == nil && validateManagementToken(value) == nil {
			return value, false, false, nil
		}
		if p == nil {
			return "", false, false, errors.New("management token command failed")
		}
		choice, choiceErr := p.guidedChoice(promptModel{Question: "Management token", Helpers: []string{"Password manager did not supply a usable token. Unlock it and retry, or enter a token for this invocation."}, Default: "cancel", Primary: []option{{"retry", "Retry password manager"}, {"paste", "Enter token with hidden input"}, {"cancel", "Cancel"}}})
		if choiceErr != nil {
			return "", false, false, choiceErr
		}
		if choice == "cancel" {
			return "", false, false, errChooseStorageAgain
		}
		if choice == "retry" {
			value, e = runPromptManagementTokenCommand(ctx, p, env, command)
			value = strings.TrimSpace(value)
			if e != nil {
				return "", false, false, errors.New("management token command failed after retry")
			}
			return value, false, false, validateManagementToken(value)
		}
	}
	if !interactive || p == nil {
		return "", false, false, errors.New("set CLOUDFLARE_API_TOKEN for this explicit provider operation")
	}
	helpers := p.tokenHelpers
	if len(helpers) == 0 {
		helpers = []string{"Create an account-owned Cloudflare API token. Creation/revocation needs Account API Tokens Write; verification needs Read or Write. Guided setup also needs Workers R2 Storage Write. Do not enter an R2 access key. The token stays in memory.", cloudflare.TokenDashboardURL}
	}
	value, e := p.guidedText(promptModel{Question: "Cloudflare API token (hidden; Enter to choose another option)", Helpers: helpers, Label: "Credential", Secret: true, Validate: func(value string) error {
		if value == "" {
			return nil
		}
		return validateManagementToken(strings.TrimSpace(value))
	}})
	if e != nil {
		return "", false, false, fmt.Errorf("could not read management token: %w", e)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false, false, errChooseStorageAgain
	}
	return value, false, false, validateManagementToken(value)
}

func validateManagementToken(value string) error {
	if len(value) == 0 || len(value) > maxManagementTokenBytes || strings.Trim(value, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~+/=") != "" {
		return errors.New("management token is empty or malformed")
	}
	return nil
}

func runPromptManagementTokenCommand(ctx context.Context, p *prompter, env Env, command []string) (string, error) {
	release := func() {}
	if p != nil {
		release = p.suspendPrompts()
	}
	defer release()
	return env.runManagementTokenCommand(ctx, command)
}
