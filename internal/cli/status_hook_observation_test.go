package cli

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestStatusUsesHookObservationIndependentlyOfAdmissionOrigin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		origin   archive.SessionOrigin
		observed bool
		want     bool
	}{
		{"discovery-without-hook", archive.SessionOriginDiscovery, false, false},
		{"discovery-resumed-by-hook", archive.SessionOriginDiscovery, true, true},
		{"import-without-hook", archive.SessionOriginImport, false, false},
		{"import-resumed-by-hook", archive.SessionOriginImport, true, true},
		{"legacy-hook", archive.SessionOriginHook, false, true},
		{"legacy-empty-origin", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, project := t.TempDir(), t.TempDir()
			now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
			cfg := pairTestConfig(now, []string{"codex"}, project)
			if err := config.Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			store, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			var hookObservedAt time.Time
			if tc.observed {
				hookObservedAt = now
			}
			reg := archive.SessionRegistration{ArchiveSessionID: "session", NativeSessionID: "native-session", ProjectID: archive.ProjectID(project), ProjectRoot: project, Harness: archive.Harness{Name: "codex"}, SessionStartedAt: now, AdmittedAt: now, Origin: tc.origin, HookObservedAt: hookObservedAt}
			if err := store.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			var view statusView
			sessions := readSessionStatus(&view, cfg, home, store)
			app := sessions.appStatus("codex", cfg, home, nil)
			pair := app.Projects[0]
			wantState, wantVerification := "waiting for first session", "not_verified"
			if !reg.Imported() && !tc.want {
				wantState = "task found; waiting for capture"
			}
			if tc.want {
				wantState, wantVerification = "hook observed; waiting for capture", "hook_observed"
			}
			if app.HookObserved != tc.want || pair.HookObserved != tc.want || app.State != wantState || pair.VerificationState != wantVerification {
				t.Fatalf("hook observation: app=%#v pair=%#v", app, pair)
			}
			if reg.Imported() && (app.ImportedSessions != 1 || app.Sessions != 0 || app.Published || pair.ReadBackVerified) {
				t.Fatalf("hook observation changed import counts or publication evidence: %#v", app)
			}
		})
	}
}
