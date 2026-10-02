package cli

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/capture"
	"time"
)

func handleTestHookEvent(home, harness string, payload map[string]any, now time.Time) error {
	decoder, ok := productionAgents.LookupDecoder(harness)
	if !ok {
		return nil
	}
	batch, err := decoder.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: now})
	if err != nil {
		return err
	}
	return capture.HandleBatch(home, harness, batch, now)
}
