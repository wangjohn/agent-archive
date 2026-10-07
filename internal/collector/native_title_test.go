package collector

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestClaudeTitleOnlyPublicationPreservesActivity(t *testing.T) {
	t.Parallel()
	for _, upgrade := range []bool{false, true} {
		for _, timestamped := range []bool{false, true} {
			t.Run(map[bool]string{false: "current filter/", true: "filter upgrade/"}[upgrade]+map[bool]string{false: "missing end", true: "recorded end"}[timestamped], func(t *testing.T) {
				t.Parallel()
				stamp := ""
				if timestamped {
					stamp = `,"timestamp":"2026-01-01T00:01:00Z"`
				}
				raw := `{"type":"user","sessionId":"native-1"` + stamp + `,"message":{"role":"user","content":"Fix widget"}}` + "\n"
				path := writeTranscript(t, t.TempDir(), "claude.jsonl", raw)
				local := newTestStore(t)
				reg := registration(t, path)
				reg.Harness = archive.Harness{Name: "claude"}
				if err := local.SaveRegistration(reg); err != nil {
					t.Fatal(err)
				}
				cloud := storagetest.NewMemoryStore()
				first := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
				observations := 0
				run := func(at time.Time) Result {
					t.Helper()
					result, err := Run(context.Background(), local, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return at }, SupplementalEvidence: func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
						observations++
						return nil, nil
					}})
					if err != nil || len(result.Errors) > 0 {
						t.Fatalf("run: %+v %v", result, err)
					}
					return result
				}
				run(first)
				if upgrade {
					simulateFilterUpgrade(t, local)
				}
				before := fetchMetadata(t, cloud, "claude", "session-1")
				names := []string{`{"type":"ai-title","aiTitle":"Generated","sessionId":"native-1"}`, `{"type":"custom-title","customTitle":"Chosen","sessionId":"native-1","timestamp":"2099-01-01T00:00:00Z"}`}
				for i, name := range names {
					raw += name + "\n"
					if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
						t.Fatal(err)
					}
					result := run(first.Add(time.Duration(i+1) * time.Hour))
					after := fetchMetadata(t, cloud, "claude", "session-1")
					if observations != 1 {
						t.Fatalf("name-only append observed new supplemental activity: %d", observations)
					}
					if after.Name != []string{"Generated", "Chosen"}[i] || len(result.Published) != 1 || after.SourceBundle == before.SourceBundle || !after.CapturedAt.Equal(before.CapturedAt) || !after.StartedAt.Equal(before.StartedAt) || !reflect.DeepEqual(after.EndedAt, before.EndedAt) || !reflect.DeepEqual(after.Counts, before.Counts) {
						t.Fatalf("title-only publication changed activity: before %+v after %+v result %+v", before, after, result)
					}
					bundle := fetchBundle(t, cloud, after)
					if len(bundle.NativeRecords) != i+2 {
						t.Fatal("title evidence absent")
					}
					before = after
				}
				if got := run(first.Add(4 * time.Hour)); len(got.Published) != 0 {
					t.Fatalf("unchanged published: %+v", got)
				}
				raw += `{"type":"user","sessionId":"native-1","message":{"role":"user","content":"Now add tests"}}` + "\n"
				if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
				run(first.Add(5 * time.Hour))
				if after := fetchMetadata(t, cloud, "claude", "session-1"); !after.CapturedAt.Equal(first.Add(5 * time.Hour)) {
					t.Fatal("new conversation did not advance capture")
				}
			})
		}
	}
}
