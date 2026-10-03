package nativecodec

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// claudeCall is a Claude Code transcript of one tool call and its result,
// on the branch fix/widget-size.
func claudeCall(t *testing.T, tool string, input map[string]any, output string, isError bool) SourceBundle {
	t.Helper()
	encodedInput, _ := json.Marshal(input)
	encodedOutput, _ := json.Marshal(output)
	return claudeLines(t,
		`{"type":"user","uuid":"u1","gitBranch":"fix/widget-size","timestamp":"2026-09-22T10:00:00Z","message":{"role":"user","content":"ship it"}}`,
		`{"type":"assistant","uuid":"a1","gitBranch":"fix/widget-size","timestamp":"2026-09-22T10:00:01Z","message":{"id":"m1","role":"assistant","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"t1","name":"`+tool+`","input":`+string(encodedInput)+`}]}}`,
		`{"type":"user","uuid":"r1","gitBranch":"fix/widget-size","timestamp":"2026-09-22T10:00:05Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":`+strconv.FormatBool(isError)+`,"content":`+string(encodedOutput)+`}]}}`,
	)
}

func gitActivityOf(t *testing.T, bundle SourceBundle) ([]GitEvent, gitCounts) {
	t.Helper()
	view, err := ParseNormalized(bundle)
	if err != nil {
		t.Fatal(err)
	}
	_ = view
	metadata, err := BuildMetadata(bundle, "machine", bundle.Capture.CapturedAt, bundle.Capture.CapturedAt, SourceReference{Key: "source", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	events := metadata.GitActivity
	counts := gitCounts{commits: deref(metadata.Counts.Commits), pushes: deref(metadata.Counts.Pushes), prsCreated: deref(metadata.Counts.PRsCreated), prsMerged: deref(metadata.Counts.PRsMerged)}
	for i := range events {
		events[i].At = nil
	}
	return events, counts
}

func TestGitActivityFromShell(t *testing.T) {
	t.Parallel()
	const pushed = "To github.com:example-org/widgets.git\n   1a2b3c4..5d6e7f8  fix/widget-size -> fix/widget-size\n"
	shell := GitEventSourceShell
	for _, tc := range []struct {
		name    string
		command string
		output  string
		isError bool
		want    []GitEvent
	}{
		{"commit", `git commit -m "Fix size"`, "[fix/widget-size 3f9c2ab] Fix size\n 1 file changed", false,
			[]GitEvent{{Kind: GitEventCommit, Source: shell, SHA: "3f9c2ab", Branch: "fix/widget-size"}}},
		{"root commit", `git commit -m init`, "[main (root-commit) 3f9c2ab] init", false,
			[]GitEvent{{Kind: GitEventCommit, Source: shell, SHA: "3f9c2ab", Branch: "main"}}},
		{"detached commit", `git -C widgets commit --amend --no-edit`, "[detached HEAD 3f9c2ab] Fix size", false,
			[]GitEvent{{Kind: GitEventCommit, Source: shell, SHA: "3f9c2ab"}}},
		{"nothing to commit", `git commit -am wip`, "On branch main\nnothing to commit, working tree clean", true, nil},
		{"commit output without the command", `git log -1`, "[fix/widget-size 3f9c2ab] Fix size", false, nil},
		{"push update", `git push`, pushed, false,
			[]GitEvent{{Kind: GitEventPush, Source: shell, SHA: "5d6e7f8", Branch: "fix/widget-size", Repository: "example-org/widgets", URL: "https://github.com/example-org/widgets/tree/fix/widget-size"}}},
		{"forced push over https with redacted userinfo", `git push --force-with-lease`, "To https://[REDACTED]@github.com/example-org/widgets.git\n + 1a2b3c4...5d6e7f8 main -> main (forced update)\n", false,
			[]GitEvent{{Kind: GitEventPush, Source: shell, SHA: "5d6e7f8", Branch: "main", Repository: "example-org/widgets", URL: "https://github.com/example-org/widgets/tree/main"}}},
		{"push through a local proxy", `git push -u origin feature`, "To http://proxy@127.0.0.1:41413/git/example-org/widgets\n * [new branch]      feature -> feature\n", false,
			[]GitEvent{{Kind: GitEventPush, Source: shell, Branch: "feature", Repository: "example-org/widgets"}}},
		{"push to a local path", `git push`, "To /tmp/remote.git\n * [new branch]      feature -> feature\n", false,
			[]GitEvent{{Kind: GitEventPush, Source: shell, Branch: "feature"}}},
		{"rejected push", `git push`, "To github.com:example-org/widgets.git\n ! [rejected]        main -> main (fetch first)\nerror: failed to push some refs", true, nil},
		{"rejected push without an error flag", `git push`, "To github.com:example-org/widgets.git\n ! [rejected]        main -> main (fetch first)\n", false, nil},
		{"up to date", `git push`, "Everything up-to-date", false, nil},
		{"dry run", `git push --dry-run`, pushed, false, nil},
		{"dry run flag of another command", `git push && echo -n done`, pushed, false,
			[]GitEvent{{Kind: GitEventPush, Source: shell, SHA: "5d6e7f8", Branch: "fix/widget-size", Repository: "example-org/widgets", URL: "https://github.com/example-org/widgets/tree/fix/widget-size"}}},
		{"push then a dry run", `git push origin main && git push --dry-run other main`,
			"To github.com:example-org/widgets.git\n   1a2b3c4..5d6e7f8  main -> main\nTo github.com:example-org/mirror.git\n   1a2b3c4..5d6e7f8  main -> main\n", false,
			[]GitEvent{{Kind: GitEventPush, Source: shell, SHA: "5d6e7f8", Branch: "main", Repository: "example-org/widgets", URL: "https://github.com/example-org/widgets/tree/main"}}},
		{"dry run then a push", `git push --dry-run other main && git push origin main`,
			"To github.com:example-org/mirror.git\n   1a2b3c4..5d6e7f8  main -> main\nTo github.com:example-org/widgets.git\n   1a2b3c4..5d6e7f8  main -> main\n", false,
			[]GitEvent{{Kind: GitEventPush, Source: shell, SHA: "5d6e7f8", Branch: "main", Repository: "example-org/widgets", URL: "https://github.com/example-org/widgets/tree/main"}}},
		{"dry run beside a push with nothing to send", `git push origin main && git push -n other main`,
			"Everything up-to-date\nTo github.com:example-org/mirror.git\n   1a2b3c4..5d6e7f8  main -> main\n", false, nil},
		{"two real pushes", `git push origin main; git push mirror main`,
			"To github.com:example-org/widgets.git\n   1a2b3c4..5d6e7f8  main -> main\nTo github.com:example-org/mirror.git\n   1a2b3c4..5d6e7f8  main -> main\n", false,
			[]GitEvent{
				{Kind: GitEventPush, Source: shell, SHA: "5d6e7f8", Branch: "main", Repository: "example-org/widgets", URL: "https://github.com/example-org/widgets/tree/main"},
				{Kind: GitEventPush, Source: shell, SHA: "5d6e7f8", Branch: "main", Repository: "example-org/mirror", URL: "https://github.com/example-org/mirror/tree/main"},
			}},
		{"push to a file url", `git push`, "To file:///tmp/remote.git\n * [new branch]      feature -> feature\n", false,
			[]GitEvent{{Kind: GitEventPush, Source: shell, Branch: "feature"}}},
		{"auto merge beside a real merge", `gh pr merge 41 --auto && gh pr merge 42 --squash`, "✓ Pull request example-org/widgets#41 will be automatically merged via squash when all requirements are met\n✓ Squashed and merged pull request example-org/widgets#42 (Fix size)\n", false,
			[]GitEvent{{Kind: GitEventPRMerged, Source: shell, Repository: "example-org/widgets", PRNumber: 42}}},
		{"push mentioned only in an echo", `echo "run git push later"`, "run git push later", false, nil},
		{"commit and push chained", `git commit -am fix && git push`, "[main 3f9c2ab] fix\nTo github.com:example-org/widgets.git\n   1a2b3c4..3f9c2ab  main -> main\n", false,
			[]GitEvent{
				{Kind: GitEventCommit, Source: shell, SHA: "3f9c2ab", Branch: "main"},
				{Kind: GitEventPush, Source: shell, SHA: "3f9c2ab", Branch: "main", Repository: "example-org/widgets", URL: "https://github.com/example-org/widgets/tree/main"},
			}},
		{"pull before push", `git pull --rebase origin feature-branch && git push`,
			"From github.com:example-org/widgets\n * branch            feature-branch -> FETCH_HEAD\n   1a2b3c4..5d6e7f8  feature-branch -> origin/feature-branch\n * [new branch]      other-branch -> origin/other-branch\nSuccessfully rebased and updated refs/heads/feature-branch.\n" + pushed, false,
			[]GitEvent{{Kind: GitEventPush, Source: shell, SHA: "5d6e7f8", Branch: "fix/widget-size", Repository: "example-org/widgets", URL: "https://github.com/example-org/widgets/tree/fix/widget-size"}}},
		{"fetch alone", `git fetch && git push --dry-run`,
			"From github.com:example-org/widgets\n   1a2b3c4..5d6e7f8  main       -> origin/main\n", false, nil},
		{"push piped through tail", `git push -u origin feature 2>&1 | tail -n 5`, "To github.com:example-org/widgets.git\n * [new branch]      feature -> feature\n", false,
			[]GitEvent{{Kind: GitEventPush, Source: shell, Branch: "feature", Repository: "example-org/widgets", URL: "https://github.com/example-org/widgets/tree/feature"}}},
		{"pr create", `gh pr create --fill`, "Creating pull request for fix/widget-size into main in example-org/widgets\n\nhttps://github.com/example-org/widgets/pull/42\n", false,
			[]GitEvent{{Kind: GitEventPRCreated, Source: shell, Branch: "fix/widget-size", Repository: "example-org/widgets", PRNumber: 42, URL: "https://github.com/example-org/widgets/pull/42"}}},
		{"pr create with a head flag", `gh pr create --fill --head feature-x`, "https://github.com/example-org/widgets/pull/43\n", false,
			[]GitEvent{{Kind: GitEventPRCreated, Source: shell, Branch: "feature-x", Repository: "example-org/widgets", PRNumber: 43, URL: "https://github.com/example-org/widgets/pull/43"}}},
		{"pr create from a fork", `gh pr create --fill -H someone:feature-x`, "https://github.com/example-org/widgets/pull/43\n", false,
			[]GitEvent{{Kind: GitEventPRCreated, Source: shell, Branch: "feature-x", Repository: "example-org/widgets", PRNumber: 43, URL: "https://github.com/example-org/widgets/pull/43"}}},
		{"pr already exists", `gh pr create --fill`, "a pull request for branch \"fix/widget-size\" into branch \"main\" already exists:\nhttps://github.com/example-org/widgets/pull/42", true, nil},
		{"pr view is not a create", `gh pr view --json url`, "https://github.com/example-org/widgets/pull/42", false, nil},
		{"pr merge naming the repository", `gh pr merge 42 --squash`, "✓ Squashed and merged pull request example-org/widgets#42 (Fix size)\n", false,
			[]GitEvent{{Kind: GitEventPRMerged, Source: shell, Repository: "example-org/widgets", PRNumber: 42}}},
		{"pr merge with a repo flag", `gh pr merge 42 --merge -R example-org/widgets`, "✓ Merged pull request #42 (Fix size)\n", false,
			[]GitEvent{{Kind: GitEventPRMerged, Source: shell, Repository: "example-org/widgets", PRNumber: 42}}},
		{"pr merge on an enterprise host", `gh pr merge 5 --squash -R ghe.example.com/example-org/widgets`, "✓ Squashed and merged pull request example-org/widgets#5 (Fix size)\n", false,
			[]GitEvent{{Kind: GitEventPRMerged, Source: shell, Repository: "example-org/widgets", PRNumber: 5, URL: "https://ghe.example.com/example-org/widgets/pull/5"}}},
		{"pr merge piped", `gh pr merge 42 --squash 2>&1 | grep -v --auto-hint`, "✓ Squashed and merged pull request example-org/widgets#42 (Fix size)\n", false,
			[]GitEvent{{Kind: GitEventPRMerged, Source: shell, Repository: "example-org/widgets", PRNumber: 42}}},
		{"pr merge with no repository", `gh pr merge 42 --rebase`, "✓ Rebased and merged pull request #42 (Fix size)\n", false,
			[]GitEvent{{Kind: GitEventPRMerged, Source: shell, PRNumber: 42}}},
		{"auto merge is not a merge", `gh pr merge 42 --auto --squash`, "✓ Pull request example-org/widgets#42 will be automatically merged via squash when all requirements are met\n", false, nil},
		{"failed merge", `gh pr merge 42`, "X Pull request example-org/widgets#42 is not mergeable: the merge commit cannot be cleanly created.", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			events, counts := gitActivityOf(t, claudeCall(t, "Bash", map[string]any{"command": tc.command}, tc.output, tc.isError))
			if !reflect.DeepEqual(events, tc.want) {
				t.Fatalf("events = %+v\nwant %+v", events, tc.want)
			}
			var want gitCounts
			for _, event := range tc.want {
				want.add(event.Kind)
			}
			if counts != want {
				t.Fatalf("counts = %+v, want %+v", counts, want)
			}
		})
	}
}

func TestGitActivityFromMCP(t *testing.T) {
	t.Parallel()
	mcp := GitEventSourceMCP
	for _, tc := range []struct {
		name    string
		tool    string
		input   map[string]any
		output  string
		isError bool
		want    []GitEvent
	}{
		{"create with url only", "mcp__github__create_pull_request",
			map[string]any{"owner": "example-org", "repo": "widgets", "title": "Fix size", "head": "fix/widget-size", "base": "main"},
			`{"id":"123","url":"https://github.com/example-org/widgets/pull/42"}`, false,
			[]GitEvent{{Kind: GitEventPRCreated, Source: mcp, Branch: "fix/widget-size", Repository: "example-org/widgets", PRNumber: 42, URL: "https://github.com/example-org/widgets/pull/42"}}},
		{"create with number only", "mcp__github__create_pull_request",
			map[string]any{"owner": "example-org", "repo": "widgets", "head": "fix/widget-size"},
			`{"number":42,"state":"open"}`, false,
			[]GitEvent{{Kind: GitEventPRCreated, Source: mcp, Branch: "fix/widget-size", Repository: "example-org/widgets", PRNumber: 42, URL: "https://github.com/example-org/widgets/pull/42"}}},
		{"create on a server not named github builds no url", "mcp__forge__create_pull_request",
			map[string]any{"owner": "example-org", "repo": "widgets"},
			`{"number":42}`, false,
			[]GitEvent{{Kind: GitEventPRCreated, Source: mcp, Repository: "example-org/widgets", PRNumber: 42}}},
		{"create failed", "mcp__github__create_pull_request",
			map[string]any{"owner": "example-org", "repo": "widgets"},
			`failed to create pull request: Validation Failed`, true, nil},
		{"merge", "mcp__github__merge_pull_request",
			map[string]any{"owner": "example-org", "repo": "widgets", "pullNumber": 42},
			`{"sha":"9e01d4c7a2b35f6e8d9c0b1a2f3e4d5c6b7a8f90","merged":true,"message":"Pull Request successfully merged"}`, false,
			[]GitEvent{{Kind: GitEventPRMerged, Source: mcp, SHA: "9e01d4c7a2b35f6e8d9c0b1a2f3e4d5c6b7a8f90", Repository: "example-org/widgets", PRNumber: 42, URL: "https://github.com/example-org/widgets/pull/42"}}},
		{"merge through the reference server", "mcp__github__merge_pull_request",
			map[string]any{"owner": "example-org", "repo": "widgets", "pull_number": 42},
			`{"sha":"9e01d4c7a2b35f6e8d9c0b1a2f3e4d5c6b7a8f90","merged":true,"message":"Pull Request successfully merged"}`, false,
			[]GitEvent{{Kind: GitEventPRMerged, Source: mcp, SHA: "9e01d4c7a2b35f6e8d9c0b1a2f3e4d5c6b7a8f90", Repository: "example-org/widgets", PRNumber: 42, URL: "https://github.com/example-org/widgets/pull/42"}}},
		{"merge reported as text", "mcp__github__merge_pull_request",
			map[string]any{"owner": "example-org", "repo": "widgets", "pullNumber": "42"},
			`Pull Request successfully merged`, false,
			[]GitEvent{{Kind: GitEventPRMerged, Source: mcp, Repository: "example-org/widgets", PRNumber: 42, URL: "https://github.com/example-org/widgets/pull/42"}}},
		{"merge not done", "mcp__github__merge_pull_request",
			map[string]any{"owner": "example-org", "repo": "widgets", "pullNumber": 42},
			`{"merged":false,"message":"Pull Request is not mergeable, not successfully merged"}`, false, nil},
		{"auto merge", "mcp__github__enable_pr_auto_merge",
			map[string]any{"owner": "example-org", "repo": "widgets", "pullNumber": 42},
			`{"enabled":true}`, false, nil},
		{"push files", "mcp__github__push_files",
			map[string]any{"owner": "example-org", "repo": "widgets", "branch": "fix/widget-size"},
			`{"ref":"refs/heads/fix/widget-size","object":{"sha":"3f9c2ab4d5e6f708192a3b4c5d6e7f8091a2b3c4","type":"commit"}}`, false,
			[]GitEvent{{Kind: GitEventCommit, Source: mcp, SHA: "3f9c2ab4d5e6f708192a3b4c5d6e7f8091a2b3c4", Branch: "fix/widget-size", Repository: "example-org/widgets"}}},
		{"create or update file", "mcp__github__create_or_update_file",
			map[string]any{"owner": "example-org", "repo": "widgets", "branch": "main", "path": "README.md"},
			`{"content":{"name":"README.md"},"commit":{"sha":"3f9c2ab4d5e6f708192a3b4c5d6e7f8091a2b3c4","message":"Update README"}}`, false,
			[]GitEvent{{Kind: GitEventCommit, Source: mcp, SHA: "3f9c2ab4d5e6f708192a3b4c5d6e7f8091a2b3c4", Branch: "main", Repository: "example-org/widgets"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			events, _ := gitActivityOf(t, claudeCall(t, tc.tool, tc.input, tc.output, tc.isError))
			if !reflect.DeepEqual(events, tc.want) {
				t.Fatalf("events = %+v\nwant %+v", events, tc.want)
			}
		})
	}
}

// A call with no result was never confirmed.
func TestGitActivityNeedsALinkedResult(t *testing.T) {
	t.Parallel()
	bundle := claudeLines(t,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-09-22T10:00:01Z","message":{"id":"m1","role":"assistant","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"git push"}}]}}`,
	)
	if events, counts := gitActivityOf(t, bundle); events != nil || counts != (gitCounts{}) {
		t.Fatalf("an unanswered call yielded %+v %+v", events, counts)
	}
}

// A merge whose confirmation names no repository takes it from the pull
// request the session created with the same number.
func TestGitActivityMergeTakesTheCreatedRepository(t *testing.T) {
	t.Parallel()
	bundle := claudeLines(t,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-09-22T10:00:01Z","message":{"id":"m1","role":"assistant","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"gh pr create --fill"}}]}}`,
		`{"type":"user","uuid":"r1","timestamp":"2026-09-22T10:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"https://github.com/example-org/widgets/pull/42"}]}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-09-22T10:00:03Z","message":{"id":"m2","role":"assistant","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"gh pr merge --squash"}}]}}`,
		`{"type":"user","uuid":"r2","timestamp":"2026-09-22T10:00:04Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"✓ Squashed and merged pull request #42 (Fix size)"}]}}`,
	)
	view, err := ParseNormalized(bundle)
	if err != nil {
		t.Fatal(err)
	}
	events := deriveGitActivity(view.ToolCalls)
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	merged := events[1]
	if merged.Kind != GitEventPRMerged || merged.Repository != "example-org/widgets" || merged.URL != "https://github.com/example-org/widgets/pull/42" {
		t.Fatalf("merge = %+v", merged)
	}
	if merged.At == nil || !merged.At.Equal(time.Date(2026, 9, 22, 10, 0, 4, 0, time.UTC)) {
		t.Fatalf("merge at = %v, want the result's timestamp", merged.At)
	}
}

// Codex's shell results carry the exit code in a JSON wrapper, and its exec
// tool is a script calling exec_command.
func TestGitActivityFromCodex(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"type":"turn_context","timestamp":"2026-09-22T10:00:00Z","payload":{"cwd":"/Users/someone/widgets","model":"gpt-6-astra"}}`,
		`{"type":"response_item","timestamp":"2026-09-22T10:00:01Z","payload":{"type":"function_call","name":"shell","call_id":"c1","arguments":"{\"command\":[\"bash\",\"-lc\",\"git commit -am fix\"]}"}}`,
		`{"type":"response_item","timestamp":"2026-09-22T10:00:02Z","payload":{"type":"function_call_output","call_id":"c1","output":"{\"output\":\"[main 3f9c2ab] fix\\n\",\"metadata\":{\"exit_code\":0}}"}}`,
		`{"type":"response_item","timestamp":"2026-09-22T10:00:03Z","payload":{"type":"function_call","name":"shell","call_id":"c2","arguments":"{\"command\":[\"bash\",\"-lc\",\"git push\"]}"}}`,
		`{"type":"response_item","timestamp":"2026-09-22T10:00:04Z","payload":{"type":"function_call_output","call_id":"c2","output":"{\"output\":\"To github.com:example-org/widgets.git\\n ! [rejected] main -> main (fetch first)\\n   1a2b3c4..5d6e7f8  main -> main\\n\",\"metadata\":{\"exit_code\":1}}"}}`,
		`{"type":"response_item","timestamp":"2026-09-22T10:00:05Z","payload":{"type":"custom_tool_call","name":"exec","call_id":"c3","input":"const r = await tools.exec_command({\"cmd\":\"gh pr create --fill\"});\nconsole.log(r);"}}`,
		`{"type":"response_item","timestamp":"2026-09-22T10:00:06Z","payload":{"type":"custom_tool_call_output","call_id":"c3","output":[{"type":"input_text","text":"Wall time: 1.2 seconds\nProcess exited with code 0\nOutput:"},{"type":"input_text","text":"https://github.com/example-org/widgets/pull/43\n"}]}}`,
	}
	filtered, err := CodexAdapter{}.FilterJSONL(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	events, counts := gitActivityOf(t, parserTestBundle(t, "codex", CodexAdapter{}, filtered))
	want := []GitEvent{
		{Kind: GitEventCommit, Source: GitEventSourceShell, SHA: "3f9c2ab", Branch: "main"},
		{Kind: GitEventPRCreated, Source: GitEventSourceShell, Repository: "example-org/widgets", PRNumber: 43, URL: "https://github.com/example-org/widgets/pull/43"},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %+v\nwant %+v", events, want)
	}
	if counts != (gitCounts{commits: 1, prsCreated: 1}) {
		t.Fatalf("counts = %+v", counts)
	}
}

// Cursor links results by position and records no timestamps.
func TestGitActivityFromCursor(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"run_terminal_cmd","input":{"command":"git commit -am fix"}}]}}`,
		`{"role":"user","message":{"content":[{"type":"tool_result","content":"[main 3f9c2ab] fix"}]}}`,
	}
	filtered, err := CursorAdapter{}.FilterJSONL(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	bundle := parserTestBundle(t, "cursor", CursorAdapter{}, filtered)
	view, err := ParseNormalized(bundle)
	if err != nil {
		t.Fatal(err)
	}
	events := deriveGitActivity(view.ToolCalls)
	want := []GitEvent{{Kind: GitEventCommit, Source: GitEventSourceShell, SHA: "3f9c2ab", Branch: "main"}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %+v\nwant %+v", events, want)
	}
}

// Beyond MaxGitActivity events the list stops and the counts go on.
func TestGitActivityCap(t *testing.T) {
	t.Parallel()
	var output strings.Builder
	for i := range MaxGitActivity + 50 {
		output.WriteString("[main " + strings.Repeat("a", 3) + "b" + strconv.FormatInt(int64(0x100000+i), 16) + "] step\n")
	}
	events, counts := gitActivityOf(t, claudeCall(t, "Bash", map[string]any{"command": "for i in $(seq 150); do git commit --allow-empty -m step; done"}, output.String(), false))
	if len(events) != MaxGitActivity || counts.commits != MaxGitActivity+50 {
		t.Fatalf("%d events, %d commits", len(events), counts.commits)
	}
}

// The git fields reach the published metadata, and a session with no git
// work publishes zero counts and no list.
func TestGitActivityInMetadata(t *testing.T) {
	t.Parallel()
	_, metadata := parsedFixture(t, "claude", "claude-git-activity.jsonl")
	countIs(t, "commits", metadata.Counts.Commits, 1)
	countIs(t, "pushes", metadata.Counts.Pushes, 1)
	countIs(t, "prs created", metadata.Counts.PRsCreated, 1)
	countIs(t, "prs merged", metadata.Counts.PRsMerged, 1)
	if len(metadata.GitActivity) != 4 || metadata.GitActivity[3].SHA != "9e01d4c7a2b35f6e8d9c0b1a2f3e4d5c6b7a8f90" {
		t.Fatalf("git activity = %+v", metadata.GitActivity)
	}
	raw, _ := json.Marshal(metadata)
	for _, text := range []string{"Fix widget size", "Commit the fix"} {
		if strings.Contains(asJSON(metadata.GitActivity), text) {
			t.Errorf("git activity carries %q: %s", text, raw)
		}
	}
	_, quiet := parsedFixture(t, "claude", "claude-tool-use.jsonl")
	countIs(t, "commits", quiet.Counts.Commits, 0)
	if quiet.GitActivity != nil {
		t.Fatalf("git activity = %+v", quiet.GitActivity)
	}
}

// A merge whose confirmation names the repository but whose host is unknown
// takes the URL of the pull request the session created on an enterprise
// host, and never a github.com URL of its own.
func TestGitActivityMergeTakesTheCreatedURL(t *testing.T) {
	t.Parallel()
	bundle := claudeLines(t,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-09-22T10:00:01Z","message":{"id":"m1","role":"assistant","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"gh pr create --fill"}}]}}`,
		`{"type":"user","uuid":"r1","timestamp":"2026-09-22T10:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"https://ghe.example.com/example-org/widgets/pull/7"}]}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-09-22T10:00:03Z","message":{"id":"m2","role":"assistant","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"gh pr merge 7 --squash"}}]}}`,
		`{"type":"user","uuid":"r2","timestamp":"2026-09-22T10:00:04Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"✓ Squashed and merged pull request example-org/widgets#7 (Fix size)"}]}}`,
		`{"type":"assistant","uuid":"a3","timestamp":"2026-09-22T10:00:05Z","message":{"id":"m3","role":"assistant","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"gh pr merge 7 --squash -R other-org/widgets"}}]}}`,
		`{"type":"user","uuid":"r3","timestamp":"2026-09-22T10:00:06Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t3","content":"✓ Squashed and merged pull request other-org/widgets#7 (Fix size)"}]}}`,
	)
	events, _ := gitActivityOf(t, bundle)
	want := []GitEvent{
		{Kind: GitEventPRCreated, Source: GitEventSourceShell, Repository: "example-org/widgets", PRNumber: 7, URL: "https://ghe.example.com/example-org/widgets/pull/7"},
		{Kind: GitEventPRMerged, Source: GitEventSourceShell, Repository: "example-org/widgets", PRNumber: 7, URL: "https://ghe.example.com/example-org/widgets/pull/7"},
		{Kind: GitEventPRMerged, Source: GitEventSourceShell, Repository: "other-org/widgets", PRNumber: 7},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %+v\nwant %+v", events, want)
	}
}

type gitCounts struct {
	commits    int
	pushes     int
	prsCreated int
	prsMerged  int
}

func (c *gitCounts) add(kind GitEventKind) {
	switch kind {
	case GitEventCommit:
		c.commits++
	case GitEventPush:
		c.pushes++
	case GitEventPRCreated:
		c.prsCreated++
	case GitEventPRMerged:
		c.prsMerged++
	}
}

func deref(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}
