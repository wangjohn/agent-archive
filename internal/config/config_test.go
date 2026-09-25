package config

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

// Every boundary compares a registration's admission, never its start. The
// matrix covers origin, admission before and after the project's activation
// and the destination boundary, and which list names the app.
func TestAcceptSessionAdmissionMatrix(t *testing.T) {
	admitted := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	yearsAgo := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	project := "/work/widget"
	// admitsHooks and admitsImports say whether the lists admit a claude
	// session of each origin; a registration without one counts as a hook's.
	type lists struct {
		harnesses     []string
		imported      []string
		admitsHooks   bool
		admitsImports bool
	}
	appLists := map[string]lists{
		"hooks":           {harnesses: []string{"claude"}, admitsHooks: true, admitsImports: true},
		"imported only":   {harnesses: []string{"codex"}, imported: []string{"claude"}, admitsImports: true},
		"neither":         {harnesses: []string{"codex"}, imported: []string{"cursor"}},
		"legacy (no app)": {admitsHooks: true, admitsImports: true},
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
					appOK := list.admitsHooks
					if origin == archive.SessionOriginImport {
						appOK = list.admitsImports
					}
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
	if reg.Imported() || reg.Origin != "" || !reg.AdmittedAt.IsZero() || reg.StartedAtSource != "" || !reg.ImportBatch.IsZero() || reg.DestinationID != "" {
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
	if err := Save(home, Config{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path(home))
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

// The destination ID covers where sessions go, never credentials.
func TestDestinationID(t *testing.T) {
	s3 := credentials.Config{Provider: "s3", Bucket: "b", Prefix: "agent-archive/", Region: "us-east-1", AWSProfile: "p"}
	other := s3
	other.AWSProfile, other.Region, other.Prefix = "q", "eu-west-1", "/agent-archive"
	if DestinationID(s3) != DestinationID(other) {
		t.Fatal("credentials or prefix spelling changed the destination")
	}
	other.Bucket = "c"
	if DestinationID(s3) == DestinationID(other) {
		t.Fatal("bucket not covered")
	}
	r2 := credentials.Config{Provider: "r2", Bucket: "b", R2AccountID: "acct", R2CredentialRef: "setup-1"}
	r2b := r2
	r2b.R2CredentialRef = "setup-2"
	if DestinationID(r2) != DestinationID(r2b) {
		t.Fatal("a credential reference changed the destination")
	}
	r2b.R2AccountID = "other"
	if DestinationID(r2) == DestinationID(r2b) {
		t.Fatal("endpoint not covered")
	}
	if (Config{Storage: r2}).DestinationID() != DestinationID(r2) {
		t.Fatal("Config.DestinationID is not the configured storage's")
	}
	if DestinationID(credentials.Config{Provider: " S3 ", Bucket: "b", Prefix: "agent-archive"}) != DestinationID(s3) {
		t.Fatal("the provider's case or spacing changed the destination")
	}
}

// Stored registrations and batch files hold these values. Changing one
// orphans every stored ID; it needs a migration, not a new expected value.
func TestDestinationIDIsPinned(t *testing.T) {
	byAccount := credentials.Config{Provider: "r2", Bucket: "archive", R2AccountID: "acct", Prefix: "agent-archive"}
	byEndpoint := credentials.Config{Provider: "r2", Bucket: "archive", R2Endpoint: "https://acct.r2.cloudflarestorage.com/", Prefix: "/agent-archive/"}
	for name, tc := range map[string]struct {
		cfg  credentials.Config
		want string
	}{
		"s3":             {credentials.Config{Provider: "s3", Bucket: "archive", Prefix: "agent-archive/", Region: "us-east-1", AWSProfile: "p"}, "2d80d2bf233cf2e71fb338eaf0948793f72efcc866fa099bd5ebb9d0ed501452"},
		"r2 by account":  {byAccount, "930af4da44d5812d1f867ebd62059c2951229d67b9e993a551a82ab7ee5f0d21"},
		"r2 by endpoint": {byEndpoint, "930af4da44d5812d1f867ebd62059c2951229d67b9e993a551a82ab7ee5f0d21"},
	} {
		if got := DestinationID(tc.cfg); got != tc.want {
			t.Errorf("%s: DestinationID=%s, want %s", name, got, tc.want)
		}
	}
}

// A registration that records its destination is judged by it, not by time:
// admitted into A, it is not accepted while the destination is B, and is
// accepted again after switching back to A, where its objects are. The
// destination's start does not matter for it; project activation still does.
func TestDestinationIDDecidesDestinationAcrossSwitchBack(t *testing.T) {
	bucketA := credentials.Config{Provider: "s3", Bucket: "a", Prefix: "agent-archive"}
	bucketB := credentials.Config{Provider: "s3", Bucket: "b", Prefix: "agent-archive"}
	admitted := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	activated := admitted.Add(-24 * time.Hour)
	configured := func(storage credentials.Config, since time.Time) Config {
		return Config{
			Storage: storage, DestinationSince: since, Harnesses: []string{"claude"},
			Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: "/p", Included: true, ActivatedAt: activated}}},
		}
	}
	for _, origin := range []archive.SessionOrigin{archive.SessionOriginHook, archive.SessionOriginImport} {
		startedAt := admitted
		if origin == archive.SessionOriginImport {
			startedAt = admitted.AddDate(-1, 0, 0)
		}
		reg := archive.SessionRegistration{ProjectRoot: "/p", Harness: archive.Harness{Name: "claude"}, Origin: origin,
			SessionStartedAt: startedAt, AdmittedAt: admitted, DestinationID: DestinationID(bucketA)}
		steps := []struct {
			name string
			cfg  Config
			want bool
		}{
			{"in A", configured(bucketA, time.Time{}), true},
			{"switched to B", configured(bucketB, admitted.Add(time.Hour)), false},
			// DestinationSince moved past the admission, which the time rule
			// would reject; the ID says the objects are in A.
			{"switched back to A", configured(bucketA, admitted.Add(2*time.Hour)), true},
		}
		for _, step := range steps {
			if got := step.cfg.AcceptSession(reg); got != step.want {
				t.Errorf("origin=%s %s: AcceptSession=%v, want %v", origin, step.name, got, step.want)
			}
			if got := step.cfg.InCurrentDestination(reg); got != step.want {
				t.Errorf("origin=%s %s: InCurrentDestination=%v, want %v", origin, step.name, got, step.want)
			}
		}
		// Project activation stays a time comparison.
		late := configured(bucketA, time.Time{})
		late.Archive.Projects[0].ActivatedAt = admitted.Add(time.Hour)
		if late.AcceptSession(reg) {
			t.Errorf("origin=%s: admitted before the project's activation but accepted", origin)
		}
		if !late.InCurrentDestination(reg) {
			t.Errorf("origin=%s: project activation changed the destination", origin)
		}
	}
}

// A registration without an ID keeps the time rule in every destination,
// even one it may have been admitted into.
func TestRegistrationWithoutDestinationIDKeepsTimeRule(t *testing.T) {
	bucketA := credentials.Config{Provider: "s3", Bucket: "a"}
	admitted := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	reg := archive.SessionRegistration{ProjectRoot: "/p", Harness: archive.Harness{Name: "claude"}, SessionStartedAt: admitted}
	back := Config{Storage: bucketA, DestinationSince: admitted.Add(time.Hour)}
	if back.AcceptSession(reg) || back.InCurrentDestination(reg) {
		t.Fatal("a registration without an ID, admitted before DestinationSince, was accepted")
	}
	back.DestinationSince = admitted.Add(-time.Hour)
	if !back.AcceptSession(reg) || !back.InCurrentDestination(reg) {
		t.Fatal("a registration without an ID, admitted after DestinationSince, was rejected")
	}
}

func TestDestinationIDRoundTripsAndOmitsWhenEmpty(t *testing.T) {
	data, err := json.Marshal(archive.SessionRegistration{})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["destination_id"]; present {
		t.Fatal("empty destination_id was written")
	}
	data, err = json.Marshal(archive.SessionRegistration{DestinationID: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	var reg archive.SessionRegistration
	if err := json.Unmarshal(data, &reg); err != nil || reg.DestinationID != "abc" {
		t.Fatalf("reg=%#v err=%v (%s)", reg, err, data)
	}
}
