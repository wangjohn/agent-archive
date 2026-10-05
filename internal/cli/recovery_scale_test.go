package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestCollectPassPreservesRecoveryFailureWhilePublishingAdmittedSession(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	home, env, _ := collectFixture(t, now)
	if err := os.WriteFile(filepath.Join(home, "session-membership.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := runPass(env, false, passOptions{})
	if err != nil || len(result.Published) != 1 {
		t.Fatalf("admitted publication under failed recovery: %#v %v", result, err)
	}
	status, err := state.OpenReadOnly(home).LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range status.LastErrors {
		if strings.Contains(problem, state.ErrSessionIndexRecoveryRequired.Error()) {
			return
		}
	}
	t.Fatalf("collector erased recovery diagnostic: %q", status.LastErrors)
}

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

// Delayed filesystem input expires the child recovery context after the final
// inventory read starts. Collection still has its independent publication time.
func TestCollectPassRetainsRecoveryCheckpointFailure(t *testing.T) {
	previousSoftDeadline := collectSoftDeadline
	collectSoftDeadline = 8 * time.Second
	t.Cleanup(func() { collectSoftDeadline = previousSoftDeadline })
	for _, broken := range []bool{false, true} {
		t.Run(strconv.FormatBool(broken), func(t *testing.T) {
			now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
			home, env, _ := collectFixture(t, now)
			reg := theRegistration(t, home)
			reg.ArchiveSessionID = "zz-delayed-owner"
			reg.NativeSessionID = "delayed-native"
			reg.ProjectID = "excluded-synthetic"
			reg.ProjectRoot = "/synthetic/excluded"
			reg.CodexAdmission = nil
			data, err := json.Marshal(reg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, "registrations", reg.ArchiveSessionID+".json")
			if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			if broken {
				if err := os.Mkdir(filepath.Join(home, "session-index-recovery.json"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			finished := make(chan error, 1)
			go func() {
				var file *os.File
				var err error
				deadline := time.Now().Add(15 * time.Second)
				for time.Now().Before(deadline) {
					file, err = os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
					if err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if err != nil {
					finished <- err
					return
				}
				// The writer's successful open proves recovery is reading the FIFO.
				time.Sleep(state.SessionIndexRecoverySlice + 100*time.Millisecond)
				replacement := path + ".replacement"
				err = os.WriteFile(replacement, data, 0600)
				if err == nil {
					err = os.Rename(replacement, path)
				}
				if err == nil {
					_, err = file.Write(data)
				}
				closeErr := file.Close()
				if err == nil {
					err = closeErr
				}
				finished <- err
			}()
			result, err := runPass(env, false, passOptions{})
			if writeErr := <-finished; writeErr != nil {
				t.Fatal(writeErr)
			}
			if err != nil || len(result.Published) != 1 {
				t.Fatalf("admitted publication: %#v %v", result, err)
			}
			status, err := state.OpenReadOnly(home).LoadStatus()
			if err != nil {
				t.Fatal(err)
			}
			if broken {
				if !strings.Contains(strings.Join(status.LastErrors, "\n"), "session-index-recovery.json") {
					t.Fatalf("checkpoint IO diagnostic lost: %q", status.LastErrors)
				}
			} else if len(status.LastErrors) != 0 {
				t.Fatalf("ordinary pending warned: %q", status.LastErrors)
			}
			var marker struct {
				Complete bool `json:"complete"`
			}
			data, err = os.ReadFile(filepath.Join(home, "session-index.json"))
			if err != nil || json.Unmarshal(data, &marker) != nil || marker.Complete {
				t.Fatalf("timed-out recovery certified: %#v %v", marker, err)
			}
		})
	}
}

// A complete census may cost more than the derived-index application allowance.
// The real CLI must still apply owners and certify recovery before publishing.
func TestCollectPassAdvancesRecoveryAfterSlowCompleteCensus(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	home, env, _ := collectFixture(t, now)
	reg := theRegistration(t, home)
	reg.ArchiveSessionID = "zz-slow-census-owner"
	reg.NativeSessionID = "slow-census-native"
	reg.ProjectID = "excluded-synthetic"
	reg.ProjectRoot = "/synthetic/excluded"
	reg.CodexAdmission = nil
	data, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "registrations", reg.ArchiveSessionID+".json")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		var file *os.File
		var err error
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			file, err = os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
			if err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			finished <- err
			return
		}
		// Successful open proves the full census reached this authoritative record.
		time.Sleep(state.SessionIndexRecoverySlice + 100*time.Millisecond)
		replacement := path + ".replacement"
		err = os.WriteFile(replacement, data, 0600)
		if err == nil {
			err = os.Rename(replacement, path)
		}
		if err == nil {
			_, err = file.Write(data)
		}
		finished <- errors.Join(err, file.Close())
	}()
	result, err := runPass(env, false, passOptions{})
	if writeErr := <-finished; writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil || len(result.Published) != 1 {
		t.Fatalf("publication after slow census: %#v %v", result, err)
	}
	var marker struct {
		Complete bool `json:"complete"`
	}
	data, err = os.ReadFile(filepath.Join(home, "session-index.json"))
	if err != nil || json.Unmarshal(data, &marker) != nil || !marker.Complete {
		t.Fatalf("full census repeatedly consumes recovery application time: %#v %v", marker, err)
	}
	store := state.OpenReadOnly(home)
	key, err := agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
	if err != nil {
		t.Fatal(err)
	}
	id, found, err := store.ArchiveSessionID(key)
	if err != nil || !found || id != reg.ArchiveSessionID {
		t.Fatalf("slow census owner: %q %v %v", id, found, err)
	}
}
