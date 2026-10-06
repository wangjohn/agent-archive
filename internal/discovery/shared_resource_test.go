package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
)

func TestSourceAndLiveIndexShareChargeBeforeCopyAndRelease(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	db := hintDatabase(t, root, true)
	addHint(t, db, "thread", "rollout", time.Now())
	mainPath := filepath.Join(root, "state_5.sqlite")
	main, err := os.OpenFile(mainPath, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := main.Truncate(96 << 20); err != nil {
		t.Fatal(err)
	}
	if err := main.Close(); err != nil {
		t.Fatal(err)
	}
	sourceDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-4111-8111-111111111111"
	path := filepath.Join(sourceDir, "rollout-2026-10-01T12-00-00-"+id+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(file, "{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q,\"timestamp\":\"2026-10-01T12:00:00Z\",\"source\":\"cli\",\"cli_version\":\"1.0.0\",\"originator\":\"codex_cli_rs\"}}\n", id, root); err != nil {
		t.Fatal(err)
	}
	row := make([]byte, 32<<10)
	for i := range len(row) - 1 {
		row[i] = ' '
	}
	row[len(row)-1] = '\n'
	for range 1025 {
		if _, err := file.Write(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	ledger := agentapi.NewNativeReadBudget(currentSnapshotLimit)
	source, err := (codex.SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{ReadBudget: ledger})
	if err != nil {
		t.Fatal(err)
	}
	captured, err := source.Read(t.Context(), agentapi.SourceRef{Path: path, Key: id}, agentapi.ReadLimits{RawBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := ledger.Charged()
	if before <= 32<<20 {
		t.Fatalf("large native source was not charged: %d", before)
	}
	copy, err := snapshotCurrentIndexBudget(t.Context(), root, nil, ledger)
	if copy != nil {
		_ = copy.close()
		t.Fatal("allocated an index copy over shared source budget")
	}
	if agentapi.Failure(err) != agentapi.Limit {
		t.Fatal(err)
	}
	used, peak := ledger.Charged()
	if used != before || peak > currentSnapshotLimit {
		t.Fatalf("failed copy leaked/exceeded shared charge: %d %d", used, peak)
	}
	if err := captured.Close(); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if used, _ := ledger.Charged(); used != 0 {
		t.Fatalf("source pass charge leaked: %d", used)
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, err = snapshotCurrentIndexBudget(ctx, root, func(stage string) {
		if stage == "generation" {
			cancel()
		}
	}, ledger)
	if err == nil {
		t.Fatal("cancelled copy accepted")
	}
	if used, _ := ledger.Charged(); used != 0 {
		t.Fatalf("cancelled copy charge leaked: %d", used)
	}
	copy, err = snapshotCurrentIndexBudget(t.Context(), root, nil, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := copy.close(); err != nil {
		t.Fatal(err)
	}
	if used, peak := ledger.Charged(); used != 0 || peak > currentSnapshotLimit {
		t.Fatalf("successful copy charge leaked/exceeded: %d %d", used, peak)
	}
}
