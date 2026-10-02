package collector

import (
	"context"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourceio"
)

func TestProviderReaderRejectsIncoherentObservations(t *testing.T) {
	for _, observation := range []agentapi.SourceObservation{
		{Present: true, Signature: agentapi.SourceSignature{Version: 1, Provider: "foreign", Token: "a"}},
		{Present: false, Signature: sourceio.FileSignature(0, 0)},
		{Present: true, Size: -1, Signature: sourceio.FileSignature(0, 0)},
	} {
		r := providerReader{reg: archive.SessionRegistration{Harness: archive.Harness{Name: "codex"}, TranscriptPath: "/synthetic/file"}, opts: Options{Sources: observationSources{observation}}}
		if _, err := r.Signature(t.Context()); err == nil {
			t.Errorf("signature accepted incoherent observation: %+v", observation)
		}
		_, filter, _ := testSources.LookupSources("codex")
		if _, observed, err := r.Filter(t.Context(), filter, DefaultMaxTranscriptBytes); err == nil || observed.observation.Present {
			t.Errorf("read accepted incoherent observation: %+v observed=%+v err=%v", observation, observed, err)
		}
	}
}

type observationSources struct{ observation agentapi.SourceObservation }

func (s observationSources) LookupSources(string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	_, f, _ := testSources.LookupSources("codex")
	return observationProvider(s), f, true
}

type observationProvider struct{ observation agentapi.SourceObservation }

func (p observationProvider) Describe(agentapi.SourceRef) (agentapi.SourceSemantics, error) {
	return agentapi.SourceSemantics{Provider: "file"}, nil
}

func (p observationProvider) OpenPass(context.Context, agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	return observationPass(p), nil
}

type observationPass struct{ observation agentapi.SourceObservation }

func (p observationPass) Signature(context.Context, agentapi.SourceRef) (agentapi.SourceObservation, error) {
	return p.observation, nil
}

func (p observationPass) Read(context.Context, agentapi.SourceRef, agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	return observationSnapshot(p), nil
}

func (observationPass) Close() error { return nil }

type observationSnapshot struct{ observation agentapi.SourceObservation }

func (s observationSnapshot) Observation() agentapi.SourceObservation { return s.observation }

func (observationSnapshot) Input() agentapi.NativeInput { return agentapi.NativeInput{} }

func (observationSnapshot) Close() error { return nil }
