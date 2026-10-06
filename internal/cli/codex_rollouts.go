package cli

import (
	"context"
	"slices"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
)

// passCodexRollouts owns one lookup for discovery and collector sources. Known
// native homes describe where facts may be read; creation permission continues
// to come independently from the actual original facts and current config.
func passCodexRollouts(ctx context.Context, store *state.Store, cfg config.Config, env Env) (*discovery.CodexRolloutLookup, error) {
	userHome, err := env.userHomeDir()
	if err != nil {
		return nil, err
	}
	homes := env.nativeSessionDirectories(userHome, cfg)["codex"]
	if cfg.Discovery != nil {
		for _, home := range cfg.Discovery.CodexHomes {
			if !slices.Contains(homes, home) {
				homes = append(homes, home)
			}
		}
	}
	return discovery.NewCodexRolloutLookup(ctx, store, homes)
}
