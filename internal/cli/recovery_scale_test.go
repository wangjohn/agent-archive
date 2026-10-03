package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/state"
)

// This explicit scale gate uses a synthetic home and an in-memory bucket. It
// exercises the real CLI ordering with 100k authoritative registrations, so
// publication must make progress while expensive recovery remains incomplete.
func TestScheduledRecoveryScalePublication(t *testing.T) {
	if os.Getenv("AGENT_ARCHIVE_RECOVERY_SCALE_TEST") != "1" {
		t.Skip("explicit synthetic 100k scale gate")
	}
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	home, env, _ := collectFixture(t, now)
	reg := theRegistration(t, home)
	reg.CodexAdmission = nil
	reg.ProjectID = "excluded-synthetic"
	reg.ProjectRoot = "/synthetic/excluded"
	for i := range 100000 {
		reg.ArchiveSessionID = fmt.Sprintf("scale-owner-%06d", i)
		reg.NativeSessionID = fmt.Sprintf("scale-native-%06d", i)
		data, err := json.Marshal(reg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "registrations", reg.ArchiveSessionID+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	local := state.OpenReadOnly(home)
	if err := local.MarkSessionIndexRecoveryNeeded(); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	done := make(chan struct{})
	var samples sync.WaitGroup
	samples.Add(1)
	var peak uint64
	go func() {
		defer samples.Done()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak {
					peak = m.HeapAlloc
				}
			}
		}
	}()
	published := false
	started := time.Now()
	result, err := runPass(env, false, passOptions{progress: func(p collector.Progress) {
		if p.Published {
			published = true
		}
	}, stop: func() bool { return published }})
	elapsed := time.Since(started)
	close(done)
	samples.Wait()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if err != nil || len(result.Published) != 1 {
		t.Fatalf("publication under recovery pressure: %#v %v", result, err)
	}
	var marker struct {
		Version  int  `json:"version"`
		Complete bool `json:"complete"`
	}
	data, err := os.ReadFile(filepath.Join(home, "session-index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &marker); err != nil || marker.Version != 1 || marker.Complete {
		t.Fatalf("recovery pressure was hidden: %#v %v", marker, err)
	}
	var cursor struct {
		Phase int `json:"phase"`
	}
	data, err = os.ReadFile(filepath.Join(home, "session-index-recovery.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.Phase < 0 || cursor.Phase > 2 {
		t.Fatalf("pending cursor: %#v %v", cursor, err)
	}
	t.Logf("actual-cli elapsed=%s pending-phase=%d sampled-peak-heap=%d stage-total-alloc=%d", elapsed, cursor.Phase, peak, after.TotalAlloc-before.TotalAlloc)
}
