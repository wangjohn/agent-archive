package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// A new desktop chat fires no sessionStart. Its first beforeSubmitPrompt has
// transcript_path null; afterAgentResponse and stop then name the transcript.
// The chat must register at the first prompt, wait without error until the
// path arrives, and then publish.
func TestCursorDesktopChatRegistersAtFirstPromptAndPublishes(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	first := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	conversation := "5f3c2a10-0000-4000-8000-00000000c3c3"
	transcript := cursorTranscriptLocation(t, conversation)

	if err := handleHookEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", conversation, project, nil), first); err != nil {
		t.Fatal(err)
	}
	reg := onlyCursorRegistration(t, home)
	if reg.NativeSessionID != conversation || reg.TranscriptPath != "" || !reg.SessionStartedAt.Equal(first) || reg.ProjectRoot != project {
		t.Fatalf("registration = %#v", reg)
	}
	if reg.Harness.Version != "3.21.13" || reg.Harness.Mode != "agent" {
		t.Fatalf("harness = %#v", reg.Harness)
	}

	// A collection pass while the path is still unknown is waiting, not failing.
	mem := storagetest.NewMemoryStore()
	env := testEnv(t, home, first.Add(time.Second))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return mem, nil }
	var stdout, stderr bytes.Buffer
	if code := runSyncCommand(nil, &stdout, &stderr, env); code != 0 || strings.Contains(stdout.String(), "1 failed") {
		t.Fatalf("waiting registration failed the pass: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	store, _ := state.Open(home)
	if status, _ := store.LoadStatus(); status.LastError != "" {
		t.Fatalf("waiting registration recorded an error: %q", status.LastError)
	}

	// Cursor writes the transcript, then names it on the response and stop.
	lines := []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"Summarize the widget package."}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"It parses widget files."}]}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := handleHookEvent(home, "cursor", cursorDesktopPayload("afterAgentResponse", conversation, project, transcript), first.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if reg = onlyCursorRegistration(t, home); reg.TranscriptPath != transcript {
		t.Fatalf("transcript path was not recorded: %#v", reg)
	}
	stop := cursorDesktopPayload("stop", conversation, project, transcript)
	stop["status"] = "completed"
	if err := handleHookEvent(home, "cursor", stop, first.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	env.Now = func() time.Time { return first.Add(time.Minute) }
	if code := runSyncCommand(nil, &stdout, &stderr, env); code != 0 || !strings.Contains(stdout.String(), "1 published") {
		t.Fatalf("chat was not published: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	key, err := archive.MetadataObjectKey("cursor", reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := mem.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Counts.Turns == nil || *metadata.Counts.Turns != 1 || metadata.Harness.Version != "3.21.13" {
		t.Fatalf("published metadata = %#v", metadata)
	}
}
