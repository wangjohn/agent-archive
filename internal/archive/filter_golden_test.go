package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

const filterGoldenPath = "testdata/filter-golden.json"

// filterGoldenEntry is what one fixture filtered to: the SHA-256 of the whole
// FilteredTranscript as JSON (records, gaps, boundary, timestamps, ids), or
// "error" when the filter refused the fixture.
type filterGoldenEntry struct {
	SHA256 string `json:"sha256,omitempty"`
	Error  bool   `json:"error,omitempty"`
}

func adapterForFixture(t *testing.T, name string) Adapter {
	t.Helper()
	harness, _, _ := strings.Cut(name, "-")
	adapter, err := NewAdapter(harness)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return adapter
}

func filterGoldenFixtures(t *testing.T) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join("testdata", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		names[i] = filepath.Base(name)
	}
	sort.Strings(names)
	return names
}

func filterGoldenOf(t *testing.T, name string) filterGoldenEntry {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	filtered, err := adapterForFixture(t, name).FilterJSONL(file)
	if err != nil {
		return filterGoldenEntry{Error: true}
	}
	encoded, err := json.Marshal(filtered)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encoded)
	return filterGoldenEntry{SHA256: hex.EncodeToString(sum[:])}
}

// The record size limit raised the scanner's maximum token size and changed
// nothing else, so it carries no FilterVersion or adapterVersion bump. That
// is only true if every fixture filters to exactly the bytes it did before;
// testdata/filter-golden.json was generated from the filter as it was before
// the change. A fixture added later is checked once its golden entry exists
// (regenerate with -update, which is itself a statement that the filter's
// output changed on purpose).
func TestFilterOutputIsUnchangedByTheRecordLimit(t *testing.T) {
	t.Parallel()
	names := filterGoldenFixtures(t)
	if golden.Update() {
		golden := map[string]filterGoldenEntry{}
		for _, name := range names {
			golden[name] = filterGoldenOf(t, name)
		}
		encoded, err := json.MarshalIndent(golden, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filterGoldenPath, append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(filterGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]filterGoldenEntry
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden) == 0 {
		t.Fatal("empty golden file")
	}
	for _, name := range names {
		want, known := golden[name]
		if !known {
			continue
		}
		if got := filterGoldenOf(t, name); got != want {
			t.Errorf("%s filtered differently: got %+v, want %+v", name, got, want)
		}
	}
}
