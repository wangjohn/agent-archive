package cli

import (
	"bytes"
	"database/sql"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCatalogProjectionSearchOracle(t *testing.T) {
	sessions := []archive.Metadata{
		{SessionID: "abcd123456789", Name: "ÉCOLE Search", Title: "reference #213", ProjectID: "one", Harness: archive.Harness{Name: "codex"}},
		{SessionID: "112233445566", Title: "abcd1234 title match", ProjectID: "two", Branch: "fix/search", Harness: archive.Harness{Name: "codex"}, PullRequests: []archive.PullRequestLink{{Number: 21}}},
		{SessionID: "abcd987654321", Name: "child", ParentSessionID: "abcd123456789", ProjectID: "one", Harness: archive.Harness{Name: "codex"}, GitActivity: []archive.GitEvent{{Kind: archive.GitEventPRCreated, PRNumber: 212}}},
	}
	var projected []archive.Metadata
	for _, m := range sessions {
		s := reader.SearchSummary{SessionID: m.SessionID, Name: m.Name, Title: m.Title, Branch: m.Branch, ProjectID: m.ProjectID, Harness: m.Harness, ParentSessionID: m.ParentSessionID, PullRequests: m.PullRequests, GitActivity: m.GitActivity}
		projected = append(projected, s.Metadata())
	}
	for _, labels := range []map[string]string{{"one": "configured École"}, {"one": "changed label", "two": "configured École"}} {
		fields := func(m archive.Metadata) sessionFields { return fieldsOf(m, sessionProjectName(m, labels)) }
		for _, word := range []string{"école", "SEARCH", "#21", "#213", "212", "abcd", "abcd1234", "configured", "missing"} {
			q := parseSessionQuery(word)
			for _, scope := range []sessionScope{{}, {Label: "one", ProjectIDs: []string{"one"}}} {
				want := searchSessions(sessions, q, scope, fields)
				got := searchSessions(projected, q, scope, fields)
				ids := func(s sessionSearch) []string {
					var out []string
					for _, m := range s.matches {
						out = append(out, m.SessionID)
					}
					return out
				}
				if !reflect.DeepEqual(ids(got), ids(want)) || got.inScope != want.inScope || got.outside != want.outside || got.subagents != want.subagents {
					t.Fatalf("word=%q scope=%+v got=%+v want=%+v", word, scope, got, want)
				}
			}
		}
	}
}

func TestPagedArchiveChoicesIncludeOlderSearchAndChildren(t *testing.T) {
	sessions := []archive.Metadata{{SessionID: "aaaaaaaa1", Harness: archive.Harness{Name: "codex"}}, {SessionID: "bbbbbbbb2", Title: "older needle", Harness: archive.Harness{Name: "codex"}}, {SessionID: "cccccccc3", ParentSessionID: "aaaaaaaa1", Title: "child needle", Harness: archive.Harness{Name: "codex"}}}
	choices := pagedArchiveChoices(sessionScope{}, listFormatOptions{}, sessions, 1, nil, "")
	if len(choices.shown().rows) != 1 || choices.shown().total != 2 || choices.loadOlder == nil {
		t.Fatal("initial page")
	}
	universe := choices.shown().search()
	rows, matched := filterRows(universe, parseSessionQuery("needle"))
	if len(rows) != 3 || matched != 2 {
		t.Fatalf("global search=%+v", rows)
	}
	more, err := choices.loadOlder.Load()
	if err != nil || more || len(choices.shown().rows) != 2 {
		t.Fatalf("older=%t err=%v rows=%d", more, err, len(choices.shown().rows))
	}
}

// BenchmarkCatalogBrowserFirstFrame compares complete synthetic list frames.
// Cold includes the first summary build; warm includes fresh canonical headers.
func BenchmarkCatalogBrowserFirstFrame(b *testing.B) {
	for _, state := range []string{"cold", "warm", "exhaustive-warm"} {
		b.Run(state, func(b *testing.B) {
			b.StopTimer()
			mem := storagetest.NewMemoryStore()
			storagetest.SeedArchive(b, mem, 800, 0)
			home := b.TempDir()
			env := benchmarkEnv(b, home, mem)
			if state == "warm" {
				if code := Run([]string{"list", "--all-projects", "--limit", "0"}, nil, io.Discard, io.Discard, env); code != 0 {
					b.Fatal("warmup failed")
				}
			}

			if state == "exhaustive-warm" {
				// Warm the body cache, then make only the disposable catalog unavailable
				// to exercise the existing exhaustive browser with cached body decoding.
				if code := Run([]string{"list", "--all-projects", "--json", "--limit", "0"}, nil, io.Discard, io.Discard, env); code != 0 {
					b.Fatal("body warmup failed")
				}
				if err := os.WriteFile(filepath.Join(home, "cache", "catalog"), []byte("unavailable catalog fixture"), 0600); err != nil {
					b.Fatal(err)
				}
			}
			var first time.Duration
			b.ReportAllocs()
			for range b.N {
				if state == "cold" {
					if err := os.RemoveAll(filepath.Join(home, "cache")); err != nil {
						b.Fatal(err)
					}
				}
				in := strings.NewReader("")
				out := &catalogBenchmarkFrame{}
				env.IsTerminal = func(stream any) bool { return stream == any(in) || stream == any(out) }
				env.TerminalSize = func(io.Writer) (int, int, bool) { return 80, 24, true }
				env.openKeys = func(io.Reader) (keyTerminal, bool) { return newFakeKeys("q"), true }
				args := []string{"list", "--all-projects"}
				b.StartTimer()
				out.start = time.Now()
				code := Run(args, in, out, io.Discard, env)
				b.StopTimer()
				if code != 0 || out.first == 0 {
					b.Fatal("no usable list frame")
				}
				first += out.first
			}
			b.ReportMetric(float64(first.Nanoseconds())/float64(b.N), "usable-screen-ns/op")
		})
	}
}

func TestCatalogWarmListSearchDecodesNoMetadataBodies(t *testing.T) {
	env, _, id := publishedFixture(t)
	reads := 0
	env.observeListBody = func(string, bool) { reads++ }
	var cold, warm bytes.Buffer
	var stderr bytes.Buffer
	args := []string{"list", id[:8], "--all-projects"}
	if code := Run(args, nil, &cold, &stderr, env); code != 0 {
		t.Fatalf("cold=%d %s", code, stderr.String())
	}
	if reads != 1 {
		t.Fatalf("cold bodies=%d", reads)
	}
	reads = 0
	stderr.Reset()
	if code := Run(args, nil, &warm, &stderr, env); code != 0 {
		t.Fatalf("warm=%d %s", code, stderr.String())
	}
	if reads != 0 {
		t.Fatalf("warm decoded=%d bodies", reads)
	}
	if cold.String() != warm.String() {
		t.Fatal("warm search changed output")
	}
}

// The key picker writes the completed table/footer/prompt in one frame. A
// spinner or alternate-screen control sequence cannot mark it usable.
type catalogBenchmarkFrame struct {
	start time.Time
	first time.Duration
}

func (s *catalogBenchmarkFrame) Write(p []byte) (int, error) {
	if s.first == 0 && bytes.Contains(p, []byte("Enter number (or unique short SESSION_ID)")) {
		s.first = time.Since(s.start)
	}
	return len(p), nil
}
func (*catalogBenchmarkFrame) colorTerminal() bool { return false }

func TestCatalogParentSearchKeepsUnmatchedChildCount(t *testing.T) {
	a := newScopedArchive(t)
	a.add(t, "parent01", "Remembered parent title", a.label)
	a.add(t, "child001", "Unrelated child task", a.label, subagentOf("parent01"))
	for range 2 {
		out, stderr, code := a.runList(t, "Remembered")
		if code != 0 || stderr != "" || !strings.Contains(out, "Remembered parent title · 1 subagent") {
			t.Fatalf("parent search: code=%d stderr=%s output=%s", code, stderr, out)
		}
	}
}

func TestCatalogDamagedSummaryFallsBackToCurrentArchive(t *testing.T) {
	env, _, id := publishedFixture(t)
	var before, after, stderr bytes.Buffer
	args := []string{"list", id[:8], "--all-projects"}
	if code := Run(args, nil, &before, &stderr, env); code != 0 {
		t.Fatalf("build=%d stderr=%s", code, stderr.String())
	}
	home, err := env.Home()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "cache", "catalog", "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("UPDATE sessions SET summary=?", []byte("damaged summary"))
	closeErr := db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	stderr.Reset()
	if code := Run(args, nil, &after, &stderr, env); code != 0 {
		t.Fatalf("fallback=%d stderr=%s", code, stderr.String())
	}
	if before.String() != after.String() {
		t.Fatal("damaged catalog hid current sessions")
	}
}

func TestCatalogWarmShowQueryReadsOnlySelectedBodyAndMatchesFallback(t *testing.T) {
	a := newScopedArchive(t)
	a.add(t, "mine0001", "Unique title ÉCOLE shared marker", a.label, linked(701))
	a.add(t, "mine0002", "Another scoped task", a.label)
	a.add(t, "bill0001", "Shared marker outside", "billing")
	a.add(t, "child001", "Child needle", a.label, subagentOf("mine0001"))
	a.add(t, "replay01", "Replay hidden title", a.label, func(m *archive.Metadata) { m.Replay = &archive.Replay{RunID: "private-replay-run"} })
	measured := storagetest.NewMeasuredStore(a.mem, 0)
	a.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return measured, nil }
	decoded := 0
	a.env.observeListBody = func(string, bool) { decoded++ }
	if _, _, code := a.runList(t, "--all-projects", "--limit", "0"); code != 0 {
		t.Fatal("catalog warmup failed")
	}
	home, err := a.env.Home()
	if err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(home, "cache", "catalog")
	for _, tc := range []struct {
		query    string
		code     int
		gets     int64
		contains string
	}{
		{"école", 0, 1, "mine0001"},
		{"#701", 0, 1, "mine0001"},
		{"mine000", 1, 0, "matches 2"},
		{"shared marker", 0, 1, "mine0001"},
		{"child needle", 0, 1, "child001"},
		{"replay01", 0, 1, "private-replay-run"},
		{"replay hidden", 1, 0, "no archived session"},
		{a.id, 0, 1, a.id},
	} {
		t.Run(tc.query, func(t *testing.T) {
			decoded = 0
			measured.Reset()
			out, stderr, code := a.runShow(t, tc.query, "--json")
			if code != tc.code || !strings.Contains(out+stderr, tc.contains) {
				t.Fatalf("code=%d out=%s stderr=%s", code, out, stderr)
			}
			if decoded != 0 || measured.Metrics().Gets != tc.gets {
				t.Fatalf("warm decoded=%d metrics=%+v", decoded, measured.Metrics())
			}
			// Preserve the warmed body cache while exercising verified exhaustive fallback.
			if err = os.Rename(catalogPath, catalogPath+".fixture"); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(catalogPath, []byte("catalog unavailable"), 0600); err != nil {
				t.Fatal(err)
			}
			fallbackOut, fallbackErr, fallbackCode := a.runShow(t, tc.query, "--json")
			if err = os.Remove(catalogPath); err != nil {
				t.Fatal(err)
			}
			if err = os.Rename(catalogPath+".fixture", catalogPath); err != nil {
				t.Fatal(err)
			}
			if fallbackCode != code || fallbackOut != out || fallbackErr != stderr {
				t.Fatalf("fallback differs: code=%d out=%s stderr=%s", fallbackCode, fallbackOut, fallbackErr)
			}
		})
	}
}
