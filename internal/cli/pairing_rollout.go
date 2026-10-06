package cli

import (
	"io"

	"github.com/wangjohn/agent-archive/internal/config"
)

// setupPairingRequest hands the buffered input to pairing after ordinary setup
// releases its lock. It is an internal redirect, not a setup failure.
type setupPairingRequest struct {
	input  io.Reader
	source io.Reader
}

// Error describes the internal setup redirect.
func (*setupPairingRequest) Error() string { return "pairing selected" }

func firstSetupPairingQuestion(opts setupOptions, stdin io.Reader, out io.Writer, env Env) (setupOptions, io.Reader, error) {
	if opts.yes || opts.pair || opts.pairFile != "" || !env.interactive(stdin) || !env.interactive(out) {
		return opts, stdin, nil
	}
	home, err := env.readHome()
	if err != nil {
		return opts, stdin, err
	}
	_, found, err := config.Load(home)
	if err != nil || found {
		return opts, stdin, err
	}
	// Resume ordinary setup and its recovery before offering another onboarding path.
	if _, draftFound, _, draftErr := readDraft(home); draftErr != nil || draftFound {
		return opts, stdin, nil
	}
	if pairingAgentRefusal(env) != nil {
		return opts, stdin, nil
	}
	p := newPrompter(stdin, out)
	paired, err := p.yesNo("Already set up on another machine? Import a pairing bundle", false)
	opts.pair = paired
	return opts, p.in, err
}
