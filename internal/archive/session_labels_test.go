package archive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// claudeLabelLines is a Claude Code transcript of one prompt and reply,
// followed by lines.
func claudeLabelLines(t *testing.T, lines ...string) SourceBundle {
	t.Helper()
	base := []string{
		`{"type":"user","uuid":"u1","sessionId":"s","timestamp":"2026-09-30T10:00:00Z","gitBranch":"main","message":{"role":"user","content":"  Rename the   widget\nparser "}}`,
		`{"type":"assistant","uuid":"a1","sessionId":"s","timestamp":"2026-09-30T10:00:05Z","gitBranch":"main","message":{"role":"assistant","content":[{"type":"text","text":"Done."}]}}`,
	}
	return claudeLines(t, append(base, lines...)...)
}

func prLink(repository string, number int) string {
	return fmt.Sprintf(`{"type":"pr-link","sessionId":"s","prNumber":"%d","prRepository":%q,"prUrl":"https://github.com/%s/pull/%d","timestamp":"2026-09-30T10:05:00Z"}`,
		number, repository, repository, number)
}

func labelsOf(t *testing.T, bundle SourceBundle) Labels {
	t.Helper()
	labels, ok := SessionLabels(bundle)
	if !ok {
		t.Fatal("SessionLabels: no prompt found")
	}
	return labels
}

func TestSessionLabelsLastCustomTitleWins(t *testing.T) {
	t.Parallel()
	bundle := claudeLabelLines(t,
		`{"type":"custom-title","customTitle":"First name","sessionId":"s"}`,
		`{"type":"custom-title","customTitle":"  Second\n  name  ","sessionId":"s"}`,
	)
	labels := labelsOf(t, bundle)
	if labels.Name != "Second name" {
		t.Fatalf("name = %q, want the last custom-title, collapsed", labels.Name)
	}
	if labels.Title != "Rename the widget parser" {
		t.Fatalf("title = %q", labels.Title)
	}
}

func TestSessionLabelsNameIsCutLikeTitle(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 100)
	labels := labelsOf(t, claudeLabelLines(t, fmt.Sprintf(`{"type":"custom-title","customTitle":%q,"sessionId":"s"}`, long)))
	if want := strings.Repeat("é", sessionTitleLimit) + "…"; labels.Name != want {
		t.Fatalf("name = %q, want %d runes and an ellipsis", labels.Name, sessionTitleLimit)
	}
}

func TestSessionLabelsWithoutANameOrLinks(t *testing.T) {
	t.Parallel()
	labels := labelsOf(t, claudeLabelLines(t))
	want := Labels{Title: "Rename the widget parser", Branch: "main"}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("labels = %#v, want %#v", labels, want)
	}
	m := parserTestMetadata(t, claudeLabelLines(t))
	if m.Name != "" || m.PullRequests != nil {
		t.Fatalf("metadata name %q pull requests %#v, want neither", m.Name, m.PullRequests)
	}
}

func TestSessionLabelsPullRequestsAreDeduplicatedInFirstLinkedOrder(t *testing.T) {
	t.Parallel()
	bundle := claudeLabelLines(t,
		prLink("example-org/widget-tools", 214),
		prLink("example-org/widget-tools", 213),
		prLink("example-org/widget-tools", 214),
		prLink("example-org/other", 214),
		// Claude Code writes the number as a string; filter 13 as an integer.
		`{"type":"pr-link","sessionId":"s","prNumber":213,"prRepository":"example-org/widget-tools","timestamp":"2026-09-30T10:06:00Z"}`,
	)
	labels := labelsOf(t, bundle)
	want := []PullRequestLink{
		{Repository: "example-org/widget-tools", Number: 214, URL: "https://github.com/example-org/widget-tools/pull/214"},
		{Repository: "example-org/widget-tools", Number: 213, URL: "https://github.com/example-org/widget-tools/pull/213"},
		{Repository: "example-org/other", Number: 214, URL: "https://github.com/example-org/other/pull/214"},
	}
	if !reflect.DeepEqual(labels.PullRequests, want) {
		t.Fatalf("pull requests = %#v, want %#v", labels.PullRequests, want)
	}
}

func TestSessionLabelsPullRequestsAreCapped(t *testing.T) {
	t.Parallel()
	var lines []string
	for n := 1; n <= MaxPullRequests+5; n++ {
		lines = append(lines, prLink("example-org/widget-tools", n))
	}
	labels := labelsOf(t, claudeLabelLines(t, lines...))
	if len(labels.PullRequests) != MaxPullRequests {
		t.Fatalf("%d pull requests, want the first %d", len(labels.PullRequests), MaxPullRequests)
	}
	if first, last := labels.PullRequests[0].Number, labels.PullRequests[MaxPullRequests-1].Number; first != 1 || last != MaxPullRequests {
		t.Fatalf("kept #%d to #%d, want #1 to #%d", first, last, MaxPullRequests)
	}
}

// A bundle that did not come through filter 13 may hold a pr-link the filter
// would have dropped or rebuilt; the parser reads only what has the shape.
func TestSessionLabelsSkipsMalformedPullRequestLinks(t *testing.T) {
	t.Parallel()
	bundle := claudeLabelLines(t)
	bundle.NativeRecords = append(bundle.NativeRecords, []map[string]any{
		{"type": "pr-link", "prNumber": float64(1), "prRepository": "no-slash"},
		{"type": "pr-link", "prNumber": float64(0), "prRepository": "example-org/widget-tools"},
		{"type": "pr-link", "prNumber": 1.5, "prRepository": "example-org/widget-tools"},
		{"type": "pr-link", "prRepository": "example-org/widget-tools"},
		{"type": "pr-link", "prNumber": float64(7), "prRepository": "example-org/widget-tools", "prUrl": "https://evil.test/x"},
		{"type": "pr-link", "prNumber": 8, "prRepository": "example-org/widget-tools"},
	}...)
	want := []PullRequestLink{
		{Repository: "example-org/widget-tools", Number: 7},
		{Repository: "example-org/widget-tools", Number: 8},
	}
	if got := labelsOf(t, bundle).PullRequests; !reflect.DeepEqual(got, want) {
		t.Fatalf("pull requests = %#v, want %#v (a URL other than the rebuilt one is not kept)", got, want)
	}
}

func TestSessionLabelsBranch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		branch string
		want   string
	}{
		{"a branch", "fix/oauth-callback", "fix/oauth-callback"},
		{"a detached checkout", "HEAD", ""},
		{"a name out of shape", "bad branch;rm", ""},
		{"none recorded", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bundle := claudeLabelLines(t)
			if tc.branch != "" {
				bundle.NativeRecords[len(bundle.NativeRecords)-1]["gitBranch"] = tc.branch
			} else {
				for _, record := range bundle.NativeRecords {
					delete(record, "gitBranch")
				}
			}
			if got := labelsOf(t, bundle).Branch; got != tc.want {
				t.Fatalf("branch = %q, want %q", got, tc.want)
			}
		})
	}
}

// The branch is the last record's, as handoff reads it: a session that moved
// from one branch to another shows the one it ended on.
func TestSessionLabelsBranchIsTheLastRecorded(t *testing.T) {
	t.Parallel()
	bundle := claudeLabelLines(t)
	bundle.NativeRecords[len(bundle.NativeRecords)-1]["gitBranch"] = "later-branch"
	labels := labelsOf(t, bundle)
	if labels.Branch != "later-branch" || labels.Branch != recordedWorkspace(bundle).Branch {
		t.Fatalf("branch = %q, handoff reads %q", labels.Branch, recordedWorkspace(bundle).Branch)
	}
}

func TestSessionLabelsCursorChatName(t *testing.T) {
	t.Parallel()
	filtered, _ := filterComposerFixture(t)
	reg := registration()
	reg.Harness = Harness{Name: "cursor"}
	bundle, err := NewSourceBundle(reg, CursorAdapter{}, filtered, time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	labels := labelsOf(t, bundle)
	if labels.Name != "Fix the widget test password=[REDACTED]" || labels.Title == "" || labels.Title == labels.Name {
		t.Fatalf("labels = %#v, want the chat's name and the first prompt apart", labels)
	}
	if m := parserTestMetadata(t, bundle); m.Name != labels.Name {
		t.Fatalf("metadata name = %q, want %q", m.Name, labels.Name)
	}
	// Only Cursor's session record names a chat; another record with a name
	// key (a tool call's) is not the chat's name.
	for _, record := range bundle.NativeRecords[1:] {
		record["name"] = "Not the chat"
	}
	if got := labelsOf(t, bundle).Name; got != labels.Name {
		t.Fatalf("name = %q after other records were named", got)
	}
}

// Both paths derive the labels with one function, so a session's row shows the
// same in the picker (built from the local transcript) and after it is
// published (built into metadata).
func TestLocalAndPublishedLabelsAreIdentical(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("wide 宽 ", 30)
	bundle := claudeLabelLines(t,
		`{"type":"user","uuid":"u2","sessionId":"s","timestamp":"2026-09-30T10:01:00Z","message":{"role":"user","content":"x"}}`,
		`{"type":"custom-title","customTitle":"Rename the widget parser","sessionId":"s"}`,
		prLink("example-org/widget-tools", 213),
	)
	bundle.NativeRecords[0]["message"] = map[string]any{"role": "user", "content": long}
	local := labelsOf(t, bundle)
	published := parserTestMetadata(t, bundle)
	got := Labels{Name: published.Name, Title: published.Title, Branch: published.Branch, PullRequests: published.PullRequests}
	if !reflect.DeepEqual(local, got) {
		t.Fatalf("local labels %#v differ from published %#v", local, got)
	}
	if runes := []rune(strings.TrimSuffix(local.Title, "…")); len(runes) != sessionTitleLimit {
		t.Fatalf("title is %d runes, want it cut at %d runes (not by display width)", len(runes), sessionTitleLimit)
	}
}

func TestSessionLabelsReportsWhetherThereIsAPrompt(t *testing.T) {
	t.Parallel()
	bundle := claudeLabelLines(t)
	bundle.NativeRecords = bundle.NativeRecords[1:]
	if _, ok := SessionLabels(bundle); ok {
		t.Fatal("a transcript with no prompt reports one")
	}
}

func TestDisplayTitleFallsBackToTitle(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		m    Metadata
		want string
	}{
		{Metadata{Name: "The name", Title: "The prompt"}, "The name"},
		{Metadata{Title: "The prompt"}, "The prompt"},
		{Metadata{Name: "  ", Title: "The prompt"}, "The prompt"},
		{Metadata{}, ""},
	} {
		if got := DisplayTitle(tc.m); got != tc.want {
			t.Errorf("DisplayTitle(%#v) = %q, want %q", tc.m, got, tc.want)
		}
	}
}

func TestLatestPRPrefersLinksThenGitActivity(t *testing.T) {
	t.Parallel()
	created := func(repository string, number int) GitEvent {
		return GitEvent{Kind: GitEventPRCreated, Source: GitEventSourceShell, Repository: repository, PRNumber: number, URL: "https://github.com/" + repository + "/pull/" + strconv.Itoa(number)}
	}
	m := Metadata{GitActivity: []GitEvent{
		created("example-org/widget-tools", 10),
		{Kind: GitEventPRMerged, Source: GitEventSourceShell, Repository: "example-org/widget-tools", PRNumber: 10},
		created("example-org/widget-tools", 11),
		{Kind: GitEventPush, Source: GitEventSourceShell, Branch: "main"},
	}}
	got, ok := LatestPR(m)
	if !ok || got.Number != 11 || got.Repository != "example-org/widget-tools" || got.URL == "" {
		t.Fatalf("LatestPR without links = %#v, %v, want the last pr_created", got, ok)
	}
	m.PullRequests = []PullRequestLink{{Repository: "example-org/widget-tools", Number: 5}, {Repository: "example-org/widget-tools", Number: 6}}
	if got, ok := LatestPR(m); !ok || got.Number != 6 {
		t.Fatalf("LatestPR with links = %#v, %v, want the last linked", got, ok)
	}
	if _, ok := LatestPR(Metadata{GitActivity: []GitEvent{{Kind: GitEventPRMerged, PRNumber: 3}}}); ok {
		t.Fatal("a merge alone is not a pull request the session opened")
	}
	if _, ok := LatestPR(Metadata{}); ok {
		t.Fatal("LatestPR of nothing")
	}
}

// The schema's pull_requests bound is MaxPullRequests, and it accepts the
// links the parser writes and nothing else in their place.
func TestMetadataSchemaBoundsPullRequestsAndBranch(t *testing.T) {
	t.Parallel()
	schema := compileSchema(t, "metadata.schema.json")
	derived := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	metadata := parserTestMetadata(t, claudeLabelLines(t))
	metadata.MetadataDerivedAt = derived
	encode := func(mutate func(doc map[string]any)) []byte {
		data, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		mutate(doc)
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	links := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = map[string]any{"repository": "example-org/widget-tools", "number": i + 1, "url": "https://github.com/example-org/widget-tools/pull/1"}
		}
		return out
	}
	accepted := func(mutate func(doc map[string]any)) bool {
		instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(encode(mutate)))
		if err != nil {
			t.Fatal(err)
		}
		return schema.Validate(instance) == nil
	}
	if !accepted(func(doc map[string]any) { doc["pull_requests"] = links(MaxPullRequests); doc["name"] = "A name" }) {
		t.Error("the schema rejects MaxPullRequests links and a name")
	}
	for name, mutate := range map[string]func(doc map[string]any){
		"too many links":        func(doc map[string]any) { doc["pull_requests"] = links(MaxPullRequests + 1) },
		"a link with no number": func(doc map[string]any) { doc["pull_requests"] = []any{map[string]any{"repository": "a/b"}} },
		"a link with a host": func(doc map[string]any) {
			doc["pull_requests"] = []any{map[string]any{"repository": "a/b", "number": 1, "url": "https://evil.test/a/b/pull/1"}}
		},
		"a link with an extra": func(doc map[string]any) {
			doc["pull_requests"] = []any{map[string]any{"repository": "a/b", "number": 1, "title": "x"}}
		},
		"a branch with a space": func(doc map[string]any) { doc["branch"] = "a b" },
		"an empty name":         func(doc map[string]any) { doc["name"] = "" },
	} {
		if accepted(mutate) {
			t.Errorf("the schema accepts %s", name)
		}
	}
}
