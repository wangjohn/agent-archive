package cli

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
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
	rows, _ := filterRows(universe, parseSessionQuery("needle"))
	if len(rows) != 2 {
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
	for _, state := range []string{"cold", "warm", "exhaustive"} {
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
			var first time.Duration
			b.ReportAllocs()
			for range b.N {
				if state == "cold" {
					if err := os.RemoveAll(filepath.Join(home, "cache")); err != nil {
						b.Fatal(err)
					}
				}
				in := strings.NewReader("")
				out := &benchmarkScreen{}
				env.IsTerminal = func(stream any) bool { return stream == any(in) || stream == any(out) }
				env.TerminalSize = func(io.Writer) (int, int, bool) { return 80, 24, true }
				env.openKeys = func(io.Reader) (keyTerminal, bool) { return newFakeKeys("q"), true }
				args := []string{"list", "--all-projects"}
				if state == "exhaustive" {
					args = append(args, "--no-cache")
				}
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
