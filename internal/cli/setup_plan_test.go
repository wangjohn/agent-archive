package cli

import (
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

func TestSetupPreflightScopeKeepsDraftAppsDuringRecovery(t *testing.T) {
	t.Parallel()
	existing := config.Config{
		Harnesses:         []string{"codex"},
		DeclinedHarnesses: []string{"cursor"},
		Storage:           credentials.Config{Provider: credentials.ProviderS3},
	}
	unfinished := setupDraft{Config: config.Config{
		Harnesses: []string{"claude"},
		Storage:   credentials.Config{Provider: credentials.ProviderR2, R2CredentialRef: "staged-ref"},
	}}
	scope := setupPreflightScope([]string{"cursor"}, existing, unfinished, true, true)
	if !reflect.DeepEqual(scope.apps, []string{"codex", "claude"}) {
		t.Fatalf("apps = %v", scope.apps)
	}
	if !reflect.DeepEqual(scope.kept, scope.apps) {
		t.Fatalf("kept = %v, want all draft apps %v", scope.kept, scope.apps)
	}
	if !scope.r2 || scope.credentialRef != "staged-ref" {
		t.Fatalf("R2 preflight = %+v", scope)
	}
	withoutDraft := setupPreflightScope([]string{"cursor"}, existing, setupDraft{}, false, true)
	if !reflect.DeepEqual(withoutDraft.kept, []string{"codex"}) {
		t.Fatalf("installed apps to keep = %v", withoutDraft.kept)
	}
}

func TestRetiredStagedRefsPreservesActiveAndOriginal(t *testing.T) {
	t.Parallel()
	original := []string{"old"}
	got := retiredStagedRefs(original, []string{"staged", "active", "staged", "old"}, "active")
	if !reflect.DeepEqual(got, []string{"old", "staged"}) {
		t.Fatalf("retired refs = %v", got)
	}
	if !reflect.DeepEqual(original, []string{"old"}) {
		t.Fatalf("input changed: %v", original)
	}
}

func TestReviewedSetupConfigUsesCommittedImportsWithoutChangingDraft(t *testing.T) {
	t.Parallel()
	existing := config.Config{ImportedHarnesses: []string{"claude", "cursor"}}
	draft := setupDraft{Config: config.Config{Harnesses: []string{"codex", "claude"}}, StopImported: []string{"cursor"}}
	got := reviewedSetupConfig(existing, draft)
	if got.RetentionDays != defaultRetentionDays {
		t.Fatalf("retention = %d", got.RetentionDays)
	}
	if len(got.ImportedHarnesses) != 0 {
		t.Fatalf("imported harnesses = %v", got.ImportedHarnesses)
	}
	if draft.Config.RetentionDays != 0 || len(draft.Config.ImportedHarnesses) != 0 {
		t.Fatalf("draft was changed: %+v", draft.Config)
	}
}
