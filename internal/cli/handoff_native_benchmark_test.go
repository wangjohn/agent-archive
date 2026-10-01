package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/nativesessions"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// BenchmarkNativeFirstPreview isolates enumeration, headers and 50 labels from
// terminal rendering and process launch. Files are synthetic; no app is started.
func BenchmarkNativeFirstPreview(b *testing.B) {
	for _, count := range []int{100, 1000, 10000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			root := b.TempDir()
			cwd := b.TempDir()
			project := filepath.Join(root, "project")
			if err := os.Mkdir(project, 0o700); err != nil {
				b.Fatal(err)
			}
			for i := range count {
				id := fmt.Sprintf("native-%05d", i)
				record, err := json.Marshal(map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "message": map[string]any{"role": "user", "content": "Fix widget callback"}})
				if err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(project, id+".jsonl"), append(record, '\n'), 0o600); err != nil {
					b.Fatal(err)
				}
			}
			roots := []nativesessions.StoreRoot{{Harness: "claude", Path: root}}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				files := &nativeReadMeter{}
				result, err := nativesessions.Discover(context.Background(), files, roots, nativesessions.Scope{Directories: []string{cwd}}, nativesessions.Limits{Files: 10000, HeaderBytes: nativeWindowBytes, RecordBytes: nativeWindowBytes, TotalBytes: nativeReadBudget, Workers: 2})
				if err != nil {
					b.Fatal(err)
				}
				catalog := &nativePreviewCatalog{previews: previewsFor(nil), ctx: context.Background(), files: files, candidates: result.Candidates, reserved: result.Coverage.ReservedBytes, stderr: io.Discard, now: time.Unix(0, 0)}
				if _, err := catalog.load(); err != nil {
					b.Fatal(err)
				}
				read, _, active, full := files.counts()
				if active != 0 || full != 0 || read > nativeReadBudget || catalog.next > 50 {
					b.Fatal("pipeline exceeded deterministic bounds")
				}
				b.ReportMetric(float64(read), "read-bytes/op")
				b.ReportMetric(float64(files.peak), "open-files")
				b.ReportMetric(float64(full), "full-filters/op")
			}
		})
	}
}
