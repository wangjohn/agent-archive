package config

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Every boundary compares a registration's admission, never its start. The
// matrix covers origin, admission before and after the project's activation
// and the destination boundary, and which list names the app.
func TestAcceptSessionAdmissionMatrix(t *testing.T) {
	admitted := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	yearsAgo := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	project := "/work/widget"
	type lists struct{ harnesses, imported []string }
	appLists := map[string]lists{
		"hooks":           {harnesses: []string{"claude"}},
		"imported only":   {harnesses: []string{"codex"}, imported: []string{"claude"}},
		"neither":         {harnesses: []string{"codex"}, imported: []string{"cursor"}},
		"legacy (no app)": {},
	}
	boundary := func(after bool) time.Time {
		if after {
			return admitted.Add(-time.Hour)
		}
		return admitted.Add(time.Hour)
	}
	for _, origin := range []archive.SessionOrigin{"", archive.SessionOriginHook, archive.SessionOriginImport} {
		for _, afterActivation := range []bool{false, true} {
			for _, afterDestination := range []bool{false, true} {
				for listName, list := range appLists {
					cfg := Config{
						DestinationSince:  boundary(afterDestination),
						Harnesses:         list.harnesses,
						ImportedHarnesses: list.imported,
						Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{
							ProjectID: archive.ProjectID(project), Root: project, Included: true, ActivatedAt: boundary(afterActivation),
						}}},
					}
					reg := archive.SessionRegistration{
						ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: archive.ProjectID(project), ProjectRoot: project,
						Harness: archive.Harness{Name: "claude"}, Origin: origin,
					}
					switch origin {
					case archive.SessionOriginImport:
						// An import's start is its true start, long before
						// every boundary; only its admission can pass them.
						reg.SessionStartedAt, reg.AdmittedAt = yearsAgo, admitted
					case archive.SessionOriginHook:
						reg.SessionStartedAt, reg.AdmittedAt = admitted, admitted
					default:
						// A registration from before AdmittedAt existed.
						reg.SessionStartedAt = admitted
					}
					appOK := listName == "hooks" || listName == "legacy (no app)" || (listName == "imported only" && origin == archive.SessionOriginImport)
					want := afterActivation && afterDestination && appOK
					name := fmt.Sprintf("origin=%q afterActivation=%v afterDestination=%v apps=%s", origin, afterActivation, afterDestination, listName)
					if got := cfg.AcceptSession(reg); got != want {
						t.Errorf("%s: AcceptSession=%v, want %v", name, got, want)
					}
				}
			}
		}
	}
}

// An imported registration's old start never decides anything: before
// this, a session that began before activation was always rejected.
func TestAcceptSessionImportUsesAdmissionNotStart(t *testing.T) {
	activated := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cfg := Config{
		DestinationSince: activated,
		Harnesses:        []string{"claude"},
		Archive:          archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: "/p", Included: true, ActivatedAt: activated}}},
	}
	reg := archive.SessionRegistration{ProjectRoot: "/p", Harness: archive.Harness{Name: "claude"}, Origin: archive.SessionOriginImport,
		SessionStartedAt: activated.AddDate(-1, 0, 0), AdmittedAt: activated.Add(time.Minute)}
	if !cfg.AcceptSession(reg) {
		t.Fatal("import admitted after activation was rejected for its old start")
	}
	reg.AdmittedAt = time.Time{}
	if cfg.AcceptSession(reg) {
		t.Fatal("a registration without AdmittedAt must fall back to its start")
	}
}

// A registration file written before the admission fields existed decodes as
// a hook registration and is judged exactly as before: by its start.
func TestLegacyRegistrationDecodesAsHookAndKeepsItsBoundary(t *testing.T) {
	activated := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	legacy := []byte(`{"archive_session_id":"a","native_session_id":"n","project_id":"p","project_root":"/p","harness":{"name":"claude"},"transcript_path":"/t.jsonl","session_started_at":"2026-09-02T00:00:00Z","registered_at":"2026-09-02T00:00:00Z"}`)
	var reg archive.SessionRegistration
	if err := json.Unmarshal(legacy, &reg); err != nil {
		t.Fatal(err)
	}
	if reg.Imported() || reg.Origin != "" || !reg.AdmittedAt.IsZero() || reg.StartedAtSource != "" || reg.ImportBatch != "" {
		t.Fatalf("legacy registration decoded with import fields: %#v", reg)
	}
	if !reg.Admitted().Equal(reg.SessionStartedAt) {
		t.Fatalf("Admitted()=%v, want the start %v", reg.Admitted(), reg.SessionStartedAt)
	}
	cfg := Config{Harnesses: []string{"claude"}, ImportedHarnesses: []string{"codex"},
		Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: "/p", Included: true, ActivatedAt: activated}}}}
	if !cfg.AcceptSession(reg) {
		t.Fatal("legacy registration after activation was rejected")
	}
	cfg.DestinationSince = time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	if cfg.AcceptSession(reg) {
		t.Fatal("legacy registration before the destination boundary was accepted")
	}
	cfg.DestinationSince = time.Time{}
	reg.Harness.Name = "codex"
	if cfg.AcceptSession(reg) {
		t.Fatal("ImportedHarnesses admitted a registration that is not an import")
	}
}

func TestImportedHarnessesRoundTripAndOmitWhenEmpty(t *testing.T) {
	home := t.TempDir()
	data, err := json.Marshal(Config{})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["imported_harnesses"]; present {
		t.Fatal("empty imported_harnesses was written")
	}
	if err := Save(home, Config{Harnesses: []string{"claude"}, ImportedHarnesses: []string{"cursor"}}); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(home)
	if err != nil || len(cfg.ImportedHarnesses) != 1 || cfg.ImportedHarnesses[0] != "cursor" {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
}

// After a destination change, a session belongs to the bucket it was admitted
// into. An import admitted after the change published here however old its
// start; a hook session from before the change did not.
func TestInCurrentDestinationComparesAdmission(t *testing.T) {
	since := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	cfg := Config{DestinationSince: since}
	importedHere := archive.SessionRegistration{Origin: archive.SessionOriginImport, SessionStartedAt: since.AddDate(-1, 0, 0), AdmittedAt: since.Add(time.Hour)}
	importedBefore := archive.SessionRegistration{Origin: archive.SessionOriginImport, SessionStartedAt: since.AddDate(-1, 0, 0), AdmittedAt: since.Add(-time.Hour)}
	legacyBefore := archive.SessionRegistration{SessionStartedAt: since.Add(-time.Hour)}
	legacyAfter := archive.SessionRegistration{SessionStartedAt: since.Add(time.Hour)}
	for name, tc := range map[string]struct {
		reg  archive.SessionRegistration
		want bool
	}{
		"import admitted after the change":  {importedHere, true},
		"import admitted before the change": {importedBefore, false},
		"legacy started before the change":  {legacyBefore, false},
		"legacy started after the change":   {legacyAfter, true},
	} {
		if got := cfg.InCurrentDestination(tc.reg); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	if !(Config{}).InCurrentDestination(importedBefore) {
		t.Error("a configuration that never changed destination has one bucket")
	}
}
