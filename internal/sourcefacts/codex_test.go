package sourcefacts

import (
	"encoding/json"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testID = "00000000-0000-0000-0000-000000000001"

func testRecords(t *testing.T, m map[string]any, task map[string]any) string {
	t.Helper()
	if m == nil {
		m = map[string]any{"id": testID, "timestamp": "2026-10-01T12:00:00Z", "cwd": "/included", "source": "cli", "originator": "synthetic", "cli_version": "test"}
	}
	if task == nil {
		task = map[string]any{"type": "task_started", "turn_id": testID, "root_turn_id": testID, "started_at": "2026-10-01T12:00:00Z"}
	}
	first, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": m})
	second, _ := json.Marshal(map[string]any{"type": "event_msg", "payload": task})
	return string(first) + "\n" + string(second) + "\n"
}
func TestCodexNativeShapeDoesNotEnableAnUnverifiedProducer(t *testing.T) {
	t.Parallel()
	h := ReadCodexHeader(strings.NewReader(testRecords(t, nil, nil)), "rollout-2026-10-01T12-00-00-"+testID+".jsonl")
	if h.Outcome != "native_format" {
		t.Fatal(h.Outcome)
	}
	if SupportedCodexProducer(h.Meta) {
		t.Fatal("unverified source activated")
	}
}
func TestCodexFirstImportedTurnNeverBecomesNativeOnResume(t *testing.T) {
	t.Parallel()
	data := testRecords(t, nil, map[string]any{"type": "task_started", "turn_id": "external-import-turn-1", "root_turn_id": nil}) + testRecords(t, nil, nil)
	h := ReadCodexHeader(strings.NewReader(data), "rollout-"+testID+".jsonl")
	if h.Outcome != "inherited_history" {
		t.Fatal(h.Outcome)
	}
}
func TestCodexInheritedAndUnknownExecutionFailClosed(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"forked_from_id", "forked_from_ordinal_exclusive", "parent_thread_id", "history_base", "subagent_history_start_ordinal", "source"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			m := map[string]any{"id": testID, "timestamp": "2026-10-01T12:00:00Z", "cwd": "/included", "source": "cli", "originator": "synthetic", "cli_version": "test"}
			m[field] = map[string]any{"content": "never copied to diagnostics"}
			h := ReadCodexHeader(strings.NewReader(testRecords(t, m, nil)), "rollout-"+testID+".jsonl")
			if h.Outcome == "native_format" {
				t.Fatal("unsupported shape admitted")
			}
		})
	}
}
func TestCodexHeaderLimitsAndIncompleteRecords(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"{", strings.Repeat("x", HeaderBytes), testRecords(t, nil, nil)[:20], strings.Repeat("{}\n", HeaderRecords+1)} {
		h := ReadCodexHeader(strings.NewReader(input), "rollout-"+testID+".jsonl")
		if h.Outcome == "native_format" || h.Bytes > HeaderBytes {
			t.Fatalf("unbounded or accepted: %#v", h)
		}
	}
}
func TestRegularSourceRejectsEscapesAndFIFOWithoutBlocking(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "secret")
	if err := os.WriteFile(target, []byte("SECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "rollout-"+testID+".jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenRegular(root, link); err == nil {
		_ = f.Close()
		t.Fatal("symlink accepted")
	}
	fifo := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { _, err := OpenRegular(root, fifo); done <- err != nil }()
	select {
	case rejected := <-done:
		if !rejected {
			t.Fatal("FIFO accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO blocked")
	}
	if _, err := OpenRegular(root, target); err == nil {
		t.Fatal("escape accepted")
	}
}

func TestMalformedFirstTaskCannotBeSkippedForANativeResume(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"root_turn_id", "turn_id"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			task := map[string]any{"type": "task_started", "turn_id": testID, "root_turn_id": testID, "started_at": "2026-10-01T12:00:00Z"}
			task[field] = map[string]any{"wrong": "JSON type"}
			data := testRecords(t, nil, task) + testRecords(t, nil, nil)
			h := ReadCodexHeader(strings.NewReader(data), "rollout-"+testID+".jsonl")
			if h.Outcome != "inherited_history" {
				t.Fatalf("malformed first task skipped: %s", h.Outcome)
			}
		})
	}
}

func TestCodexMalformedPreStartAndInconsistentTaskTimesFailClosed(t *testing.T) {
	t.Parallel()
	valid := testRecords(t, nil, nil)
	meta := strings.SplitN(valid, "\n", 2)[0] + "\n"
	for _, event := range []string{
		`{"type":"event_msg","payload":{"type":[],"turn_id":"ignored"}}`,
		`{"type":"event_msg","payload":null}`,
		`{"type":{},"payload":{"type":"task_started"}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"` + testID + `","root_turn_id":"` + testID + `","started_at":"2026-10-01T11:59:00Z"}}`,
	} {
		h := ReadCodexHeader(strings.NewReader(meta+event+"\n"+strings.SplitN(valid, "\n", 2)[1]), "rollout-"+testID+".jsonl")
		if h.Outcome == "native_format" {
			t.Fatal("rejected first event repaired by later native start")
		}
	}
}

func TestFirstTaskAfterLongIdleRetainsSeparateNativeTimestamps(t *testing.T) {
	t.Parallel()
	task := map[string]any{"type": "task_started", "turn_id": testID, "root_turn_id": testID, "started_at": "2026-10-02T12:00:00Z"}
	h := ReadCodexHeader(strings.NewReader(testRecords(t, nil, task)), "rollout-"+testID+".jsonl")
	if h.Outcome != "native_format" || h.FirstTaskAt.Sub(h.Started) != 24*time.Hour {
		t.Fatalf("ordinary idle gap rejected: %#v", h)
	}
}
