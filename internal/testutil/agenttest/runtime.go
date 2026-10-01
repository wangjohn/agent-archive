// Package agenttest supplies conformance checks used by native integrations.
package agenttest

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"reflect"
	"slices"
	"testing"
)

// RuntimeConformance checks positive evidence independently of cleanup inventory.
func RuntimeConformance(t *testing.T, detector agentapi.RuntimeDetector, positive string, exact bool, cleanup []string) {
	t.Helper()
	for _, value := range []string{"", "  ", "  native-id  ", "0"} {
		t.Run(value, func(t *testing.T) {
			env := agentapi.RuntimeEnvironment{LookupEnv: func(key string) (string, bool) {
				if key != positive {
					t.Errorf("unexpected runtime key %s", key)
				}
				return value, true
			}}
			got := detector.Detect(env)
			want := agentapi.RuntimeObservation{}
			if value == "  native-id  " || value == "0" {
				want.PresenceKey = positive
				if exact {
					want.NativeID = "native-id"
					if value == "0" {
						want.NativeID = "0"
					}
				} else {
					want.ProjectLatest = true
				}
			}
			if got != want {
				t.Fatalf("got %+v want %+v", got, want)
			}
		})
	}
	if got := detector.Detect(agentapi.RuntimeEnvironment{LookupEnv: func(string) (string, bool) { return "native", false }}); got != (agentapi.RuntimeObservation{}) {
		t.Fatalf("unset evidence %+v", got)
	}
	if got := detector.SessionEnvironmentKeys(); !reflect.DeepEqual(got, cleanup) {
		t.Fatalf("cleanup %q want %q", got, cleanup)
	}
	copied := detector.SessionEnvironmentKeys()
	copied[0] = "MUTATED"
	if slices.Contains(detector.SessionEnvironmentKeys(), "MUTATED") {
		t.Fatal("cleanup keys are mutable")
	}
	for _, key := range cleanup {
		if key == positive {
			continue
		}
		got := detector.Detect(agentapi.RuntimeEnvironment{LookupEnv: func(k string) (string, bool) { return "x", k == key }})
		if got != (agentapi.RuntimeObservation{}) {
			t.Fatalf("cleanup-only key %s is positive %+v", key, got)
		}
	}
}
