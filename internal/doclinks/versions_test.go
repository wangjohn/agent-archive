package doclinks

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// TestVersionsDocMatchesCode fails when dev/maintainers/versions.md's
// "Current" column disagrees with the constants a build records, so a
// version bump that forgets the doc is caught in review.
func TestVersionsDocMatchesCode(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "dev", "maintainers", "versions.md"))
	if err != nil {
		t.Fatal(err)
	}
	current := map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		cells := strings.Split(line, "|")
		// | Version | Where | Recorded in | Current | Changes when |
		if len(cells) != 7 {
			continue
		}
		current[strings.TrimSpace(cells[1])] = strings.TrimSpace(cells[4])
	}
	want := map[string]string{
		"Filter":          archive.FilterVersion,
		"Adapter":         archive.ClaudeAdapter{}.Version(),
		"Parser":          archive.DefaultParserVersion,
		"Source schema":   strconv.Itoa(archive.SourceSchemaVersion),
		"Metadata schema": strconv.Itoa(archive.MetadataSchemaVersion),
		"Configuration":   strconv.Itoa(config.SchemaVersion),
	}
	for row, value := range want {
		if got, ok := current[row]; !ok || got != value {
			t.Errorf("versions.md %s row says %q; the code says %q", row, got, value)
		}
	}
}
