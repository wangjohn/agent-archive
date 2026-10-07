package cli

import (
	"io"

	"github.com/wangjohn/agent-archive/internal/config"
)

// setupPairingRedirectError hands the buffered input to pairing after ordinary setup
// releases its lock. It is an internal redirect, not a setup failure.
type setupPairingRedirectError struct {
	input  io.Reader
	source io.Reader
}

// Error describes the internal setup redirect.
func (*setupPairingRedirectError) Error() string { return "pairing selected" }

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
	_, draftFound, _, draftErr := readDraft(home)
	pairingEligible := !draftFound && draftErr == nil && pairingAgentRefusal(env) == nil
	if !pairingEligible {
		return opts, stdin, nil
	}
	p := newPrompter(stdin, out)
	defer p.close()
	paired, err := p.setupYesNo("Already set up on another machine? Import a pairing bundle", false)
	opts.pair = paired
	return opts, p.in, err
}
