package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/purge"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestPurgePlanAndApplyRequirePauseAndKeepCurrentSource(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, t.TempDir(), time.Now())
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	env := testEnv(t, home, now)
	bucket := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return bucket, nil }
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	current := "sessions/codex/" + id + "/source." + strings.Repeat("a", 64) + ".jsonl.gz"
	orphan := "sessions/codex/" + id + "/source." + strings.Repeat("b", 64) + ".jsonl.gz"
	meta := archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion, SourceBundle: archive.SourceReference{Key: current, SHA256: strings.Repeat("a", 64)}, FilterVersion: "9"}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string][]byte{current: []byte("current"), orphan: []byte("orphan"), "sessions/codex/" + id + "/metadata.json": data} {
		if err := bucket.Put(context.Background(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"purge", "plan"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("plan code %d: %s", code, &errOut)
	}
	if !strings.Contains(out.String(), orphan) || !strings.Contains(out.String(), "Still-current older-filter sessions (1;") {
		t.Fatalf("plan output: %s", &out)
	}
	var planFile string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "Plan: ") {
			planFile = strings.TrimPrefix(line, "Plan: ")
		}
	}
	if planFile == "" {
		t.Fatal("missing plan path")
	}
	info, err := os.Stat(planFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("plan permissions: %v %v", info, err)
	}
	var plan purge.Plan
	if err := jsonFile(planFile, &plan); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"purge", "apply", planFile, "--yes"}, nil, &out, &errOut, env); code == 0 || !strings.Contains(errOut.String(), "pause") {
		t.Fatalf("unpaused apply code %d: %s", code, &errOut)
	}
	if _, err := config.SetPaused(home, true); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"purge", "apply", planFile, "--yes"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("apply code %d: %s", code, &errOut)
	}
	if _, err := bucket.Get(context.Background(), orphan); err == nil {
		t.Fatal("orphan survived")
	}
	if _, err := bucket.Get(context.Background(), current); err != nil {
		t.Fatalf("current removed: %v", err)
	}
	if _, err := os.Stat(planFile + ".report"); err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(planFile) != filepath.Join(home, "purge-plans") {
		t.Fatalf("unexpected plan path: %s", planFile)
	}
}

func jsonFile(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
