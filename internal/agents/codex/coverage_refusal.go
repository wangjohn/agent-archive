package codex

import "github.com/wangjohn/agent-archive/internal/agentapi"

// coverageIncomplete is the refusal the source gives while the rollout lookup
// has not yet proven which rollout files make up a thread's history: its
// lineage cannot be selected, a retained proof may miss a newer segment, or
// its historical candidates cannot be listed. It is named at its source with
// agentapi.CauseHistoryCoverage, so the collector defers the scan until a
// later pass completes coverage instead of failing it.
func coverageIncomplete(message string) error {
	return agentapi.WithCause(sourceFailure(agentapi.Unavailable, message), agentapi.CauseHistoryCoverage)
}
