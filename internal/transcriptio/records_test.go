package transcriptio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordWindowsSkipFragmentsAndOversizedRecords(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "records")
	raw := "{\"id\":1}\n" + strings.Repeat("x", 100) + "\n{\"id\":2}\nunfinished"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(OS{}, path, OpenPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var records []string
	w, err := s.Records(context.Background(), false, int64(len(raw)), 32, func(r []byte) bool { records = append(records, string(r)); return true })
	if err != nil || len(records) != 2 || w.Skipped != 2 || w.Complete || w.Bytes > int64(len(raw)) {
		t.Fatalf("records=%v window=%+v err=%v", records, w, err)
	}
	records = nil
	w, err = s.Records(context.Background(), true, 25, 32, func(r []byte) bool { records = append(records, string(r)); return true })
	if err != nil || len(records) != 1 || records[0] != "{\"id\":2}" || w.Bytes > 26 {
		t.Fatalf("tail=%v window=%+v err=%v", records, w, err)
	}
}

func TestRecordWindowCancellationAndEarlyStopCoverage(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "records")
	if err := os.WriteFile(path, []byte("{}\n{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(OS{}, path, OpenPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	w, err := s.Records(context.Background(), false, 100, 100, func([]byte) bool { return false })
	if err != nil || w.Complete {
		t.Fatalf("early stop %+v %v", w, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.Records(ctx, false, 100, 100, func([]byte) bool { t.Fatal("visited after cancellation"); return true })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
