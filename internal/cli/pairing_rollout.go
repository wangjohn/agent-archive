package cli

import (
	"io"

	"github.com/wangjohn/agent-archive/internal/config"
)

// Pairing GA requires recorded combined and live Cloudflare acceptance. Neither
// an environment variable nor an experimental opt-in activates this release gate.
func pairingGeneralAvailabilityReady() bool { return false }

func firstSetupPairingQuestion(ready bool, opts setupOptions, stdin io.Reader, out io.Writer, env Env) (setupOptions, io.Reader, error) {
	if !ready || opts.yes || opts.pair || opts.pairFile != "" || !env.interactive(stdin) {
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
	p := newPrompter(stdin, out)
	paired, err := p.yesNo("Already set up on another machine? Import a pairing bundle", false)
	opts.pair = paired
	return opts, p.in, err
}

func initialSetupPairingQuestion(maintenance bool, opts setupOptions, stdin io.Reader, out io.Writer, env Env) (setupOptions, io.Reader, error) {
	if maintenance {
		return opts, stdin, nil
	}
	return firstSetupPairingQuestion(pairingGeneralAvailabilityReady(), opts, stdin, out, env)
}
