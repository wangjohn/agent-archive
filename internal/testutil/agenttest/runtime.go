// Package agenttest supplies conformance checks used by native integrations.
package agenttest

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"reflect"
	"slices"
	"testing"
)

type runtimeValue string

const (
	unsetValue          runtimeValue = ""
	whitespaceValue     runtimeValue = "  "
	paddedIdentityValue runtimeValue = "  native-id  "
	zeroValue           runtimeValue = "0"
)

// RuntimeConformance checks positive evidence independently of cleanup inventory.
func RuntimeConformance(t *testing.T, detector agentapi.RuntimeDetector, positive string, exact bool, cleanup []string) {
	t.Helper()
	for _, value := range []runtimeValue{unsetValue, whitespaceValue, paddedIdentityValue, zeroValue} {
		t.Run(string(value), func(t *testing.T) {
			env := agentapi.RuntimeEnvironment{LookupEnv: func(key string) (string, bool) {
				if key != positive {
					t.Errorf("unexpected runtime key %s", key)
				}
				return string(value), true
			}}
			got := detector.Detect(env)
			nativeID, presenceKey := "", ""
			projectLatest := false
			if value == paddedIdentityValue || value == zeroValue {
				presenceKey = positive
				if exact {
					nativeID = "native-id"
					if value == zeroValue {
						nativeID = "0"
					}
				} else {
					projectLatest = true
				}
			}
			want := agentapi.RuntimeObservation{NativeID: nativeID, PresenceKey: presenceKey, ProjectLatest: projectLatest}
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
