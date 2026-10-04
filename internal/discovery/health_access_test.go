package discovery

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// The run-owned syscall measurement invokes this entry point with synthetic
// state already prepared, so the measured child executes only the real reader.
func TestBoundedHealthReaderAccessProbe(t *testing.T) {
	home := os.Getenv("AGENT_ARCHIVE_HEALTH_ACCESS_PROBE_HOME")
	if home == "" {
		t.Skip("run-owned synthetic syscall probe")
	}
	probeID, err := strconv.ParseInt(home, 10, 64)
	if err != nil || probeID <= 0 {
		t.Fatal("access probe requires a positive synthetic directory token")
	}
	home = filepath.Join(string(filepath.Separator), "tmp", "u-reader-probe-"+strconv.FormatInt(probeID, 10))
	h, found, err := ReadHealth(home)
	if err != nil || !found || !h.Enabled || h.LastAttempt.IsZero() {
		t.Fatalf("bounded health probe: %+v %t %v", h, found, err)
	}
}
