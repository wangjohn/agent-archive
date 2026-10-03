package capture

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"maps"
	"time"
)

var testDecoders = builtin.NewBuiltins()

type hookEventKind uint8

const (
	hookEventIgnored hookEventKind = iota
	hookEventStart
	hookEventTurnStart
	hookEventStop
	hookEventSubagentStop
	hookEventResponse
)

func testBatch(harness string, payload map[string]any, now time.Time) ([]agentapi.LifecycleEvent, error) {
	d, ok := testDecoders.LookupDecoder(harness)
	if !ok {
		return nil, nil
	}
	return d.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: now})
}

func classifyHookEvent(harness, name string) hookEventKind {
	batch, err := testBatch(harness, map[string]any{"hook_event_name": name, "session_id": "test"}, time.Now())
	if err != nil || len(batch) == 0 {
		return hookEventIgnored
	}
	// A Cursor prompt's native characterization remains turn-start classification.
	event := batch[len(batch)-1]
	switch event.Kind {
	case agentapi.EventStart:
		return hookEventStart
	case agentapi.EventTurnStart:
		return hookEventTurnStart
	case agentapi.EventResponse:
		return hookEventResponse
	case agentapi.EventStop:
		return hookEventStop
	case agentapi.EventSubagent:
		return hookEventSubagentStop
	}
	return hookEventIgnored
}

func handleEvent(home, harness string, payload map[string]any, now time.Time, lock lockHooks, afterLock func(), repoKey RepoKeyFunc) error {
	batch, err := testBatch(harness, payload, now)
	if err != nil {
		return err
	}
	return handleBatch(home, harness, batch, now, lock, afterLock, eventOptions{repoKey: repoKey, decoders: testDecoders})
}

func provesFreshSessionStart(harness string, payload map[string]any) bool {
	observed := map[string]any{}
	maps.Copy(observed, payload)
	name := "SessionStart"
	if archive.CanonicalHarness(harness) == "cursor" {
		name = "sessionStart"
	}
	observed["hook_event_name"] = name
	observed["session_id"] = "test"
	batch, err := testBatch(harness, observed, time.Now())
	return err == nil && len(batch) > 0 && resolveFreshness(batch, nil)[0].Start.Kind == agentapi.FreshExplicit
}

func filteredHookEvidence(kind archive.SupplementalEvidenceKind, harness, name string, payload map[string]any, _ bool, now time.Time) (*archive.SupplementalEvidence, error) {
	observed := map[string]any{}
	maps.Copy(observed, payload)
	observed["hook_event_name"] = name
	observed["session_id"] = "test"
	batch, err := testBatch(harness, observed, now)
	if err != nil {
		return nil, err
	}
	batch, err = validateBatch(harness, batch, now)
	if err != nil {
		return nil, err
	}
	for _, event := range batch {
		for _, e := range event.Evidence {
			if e.Kind == kind {
				return &e, nil
			}
		}
	}
	return nil, nil
}

func hookAdmissionIntent(home, harness string, _ hookEventKind, payload map[string]any, now time.Time) (admissionIntent, bool, error) {
	batch, err := testBatch(harness, payload, now)
	if err != nil {
		return admissionIntent{}, false, err
	}
	batch, err = validateBatch(harness, batch, now)
	if err != nil {
		return admissionIntent{}, false, err
	}
	return eventAdmissionIntent(home, resolveFreshness(batch, nil), now)
}

func stageAdmissionIntent(home string, intent admissionIntent, _ map[string]any, path string, stage func(string, any) (*local.Staged, error)) (*local.Staged, error) {
	return stageEventIntent(home, intent, path, stage)
}

func queueAdmissionIntentWithGeneration(home, harness string, _ hookEventKind, payload map[string]any, now time.Time, after func(), generation *string) (bool, error) {
	batch, err := testBatch(harness, payload, now)
	if err != nil {
		return false, err
	}
	batch, err = validateBatch(harness, batch, now)
	if err != nil {
		return false, err
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		return false, err
	}
	gen := cfg.PauseGeneration
	if generation != nil {
		gen = *generation
	}
	return queueEventBatchInGeneration(home, resolveFreshness(batch, nil), now, gen, nil, after)
}

// credentialsTestConfig is a syntactically valid storage destination for
// the configurations these tests save. The hook never touches storage; it
// writes only local files.
func credentialsTestConfig() credentials.Config {
	return credentials.Config{Provider: credentials.ProviderS3, Bucket: "test-bucket", Region: "us-east-1", AWSProfile: "test", Prefix: "agent-archive/"}
}

func emptyTranscriptProvesFreshStart(payload map[string]any) bool {
	path, _ := payload["transcript_path"].(string)
	event := agentapi.LifecycleEvent{Start: agentapi.StartEvidence{Kind: agentapi.FreshStat, Path: path}}
	return resolveFreshness([]agentapi.LifecycleEvent{event}, nil)[0].Start.Kind == agentapi.FreshExplicit
}

func WithDecoders(d agentapi.DecodersLookup) Option { return func(o *eventOptions) { o.decoders = d } }

func HandleEvent(home, harness string, payload map[string]any, now time.Time, options ...Option) error {
	var o eventOptions
	for _, option := range options {
		option(&o)
	}
	if payload == nil {
		return nil
	}
	if o.decoders == nil {
		return errors.New("lifecycle decoder lookup required")
	}
	decoder, ok := o.decoders.LookupDecoder(harness)
	if !ok {
		return nil
	}
	batch, err := decoder.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: now})
	if err != nil {
		return err
	}
	return handleBatch(home, harness, batch, now, nil, nil, o)
}
