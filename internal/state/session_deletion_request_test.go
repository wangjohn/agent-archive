package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRetentionIntentChecksDecisionTokenAfterPreparedFileSync(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "no prior request", true: "excluded prior request"}[known], func(t *testing.T) {
			s, reg, _ := stageFixture(t)
			if err := s.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			token := ""
			if known {
				if err := s.SaveRequest(reg.ArchiveSessionID, "prompt", reg.Admitted()); err != nil {
					t.Fatal(err)
				}
				req, _, err := s.LoadRequest(reg.ArchiveSessionID)
				if err != nil {
					t.Fatal(err)
				}
				token = req.Token
			}
			var hookErr error
			s.onDeletionBeforeCommit = func() error {
				entries, err := os.ReadDir(filepath.Join(s.home, "session-deletions"))
				if err != nil || len(entries) != 1 {
					t.Fatal("hook did not exercise prepared intent boundary", entries, err)
				}
				hookErr = s.SaveRequest(reg.ArchiveSessionID, "stop", reg.Admitted().Add(time.Hour))
				return hookErr
			}
			if _, err := s.PrepareRetentionDeletion(reg, nil, reg.Admitted().Add(time.Hour), token); !errors.Is(err, ErrDeletionWorkChanged) {
				t.Fatal("new work became deletion authority", err)
			}
			if hookErr != nil {
				t.Fatal(hookErr)
			}
			if _, found, err := s.LoadSessionDeletion(reg); err != nil || found {
				t.Fatal("rejected intent committed", found, err)
			}
			req, found, err := s.LoadRequest(reg.ArchiveSessionID)
			if err != nil || !found || req.Token == token {
				t.Fatal("newer request was lost", req, err)
			}
			if actual, err := s.deletionControlUsage(); err != nil || actual != 0 {
				t.Fatal("prepared control not reconciled", actual, err)
			}
		})
	}
}

func TestRetentionIntentKnownDecisionWorkAndNativeAbsentRemovalFacet(t *testing.T) {
	s, reg, _ := stageFixture(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRequest(reg.ArchiveSessionID, "prompt", reg.Admitted()); err != nil {
		t.Fatal(err)
	}
	req, _, err := s.LoadRequest(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.PrepareRetentionDeletion(reg, nil, reg.Admitted().Add(time.Hour), req.Token)
	if err != nil || !j.CoveredRequest || j.RequestToken != req.Token {
		t.Fatal("decision request not bound", j, err)
	}
	work, err := s.Outstanding(reg, true)
	if err != nil || !work.Removal || work.Upload || !work.Owed() || !work.Pending() || !work.SyncCanFinish() {
		t.Fatal("native-absent removal not resumable", work, err)
	}
	if (Outstanding{Removal: true, WaitingForTranscript: true}).DefersExpiry() {
		t.Fatal("removal deferred its own expiry")
	}
	if !(Outstanding{Removal: true, WaitingForTranscript: true}).SyncCanFinish() {
		t.Fatal("missing native hid removal")
	}
}

func TestRetentionIntentFailedCommitAndCleanupRetainsChargedRecovery(t *testing.T) {
	s, reg, _ := stageFixture(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("synthetic pre-rename failure")
	cleanup := errors.New("synthetic retained temp cleanup failure")
	s.onDeletionBeforeCommit = func() error { return injected }
	s.onDeletionCleanup = func() error { return cleanup }
	if _, err := s.PrepareRetentionDeletion(reg, nil, reg.Admitted(), ""); !errors.Is(err, injected) || !errors.Is(err, cleanup) {
		t.Fatal("intent failure swallowed", err)
	}
	s.onDeletionBeforeCommit = nil
	s.onDeletionCleanup = nil
	restarted, err := Open(s.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := restarted.LoadSessionDeletion(reg); !found || !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal("orphan temp invented durable authority", found, err)
	}
	if actual, err := restarted.deletionControlUsage(); err != nil || actual <= 0 {
		t.Fatal("retained control uncharged", actual, err)
	}
	if _, err := restarted.PrepareRetentionDeletion(reg, nil, reg.Admitted(), ""); !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal("orphan temp allowed fresh intent", err)
	}
}

func TestRetentionIntentDirectorySyncFailureNeverReportsDurableSuccess(t *testing.T) {
	s, reg, _ := stageFixture(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("synthetic post-rename directory sync failure")
	s.onDeletionSync = func() error { return injected }
	if _, err := s.PrepareRetentionDeletion(reg, nil, reg.Admitted(), ""); !errors.Is(err, injected) {
		t.Fatal("undurable intent reported success", err)
	}
	s.onDeletionSync = nil
	j, found, err := s.LoadSessionDeletion(reg)
	if err != nil || !found || j.Phase != DeletionPrepared {
		t.Fatal("sync failure implied deletion", j, err)
	}
	if err := s.AdvanceSessionDeletion(reg, DeletionDeleting); err != nil {
		t.Fatal("fresh durable retry failed", err)
	}
}
