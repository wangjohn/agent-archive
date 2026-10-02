package agenttest

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"reflect"
	"testing"
)

// ImportCase supplies provider-owned retained evidence and expected observations.
type ImportCase struct {
	Name    string
	Request agentapi.ImportInspectionRequest
	Want    agentapi.ImportInspection
}

// HistoricalImport checks native observations and bounded cancellation without
// imposing a shared native vocabulary or choosing shared admission outcomes.
func HistoricalImport(t *testing.T, p agentapi.ImportInspector, cases []ImportCase) {
	t.Helper()
	for _, fixture := range cases {
		t.Run(fixture.Name, func(t *testing.T) {
			got, err := p.InspectImport(t.Context(), fixture.Request)
			if err != nil || !reflect.DeepEqual(got, fixture.Want) {
				t.Fatalf("observed %+v err=%v want %+v", got, err, fixture.Want)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := p.InspectImport(ctx, fixture.Request); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
		})
	}
}
