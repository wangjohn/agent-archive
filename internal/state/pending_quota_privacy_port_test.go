package state

import (
	"errors"
	"os"
	"testing"
)

func TestPendingRAMOnlyPrivacyHandleUsesHeldStageAllowance(t *testing.T) {
	t.Parallel()
	s, reg, p := saturatedStagePending(t)
	handle, err := NewTemporaryReservation(s, PublicationPrivacy, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SavePendingWithTemporaryReservation(handle, reg.ArchiveSessionID, p); err != nil {
		t.Fatal("zero scratch consumer deadlocked", err)
	}
	if err = handle.Close(); err != nil {
		t.Fatal(err)
	}
	if err = handle.Close(); err != nil {
		t.Fatal("zero close is not idempotent", err)
	}
	if _, err = os.Stat(handle.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("RAM consumer allocated scratch", err)
	}
}

func TestPendingRAMOnlyPrivacyHandleRejectsInvalidOwnership(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"owner", "key", "root", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			s, reg, p := saturatedStagePending(t)
			owner, key := PublicationPrivacy, reg.ArchiveSessionID
			if kind == "owner" {
				owner = CursorAdmission
			}
			if kind == "key" {
				key = "different"
			}
			handle, err := NewTemporaryReservation(s, owner, key)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				closeErr := handle.Close()
				if kind == "corrupt" {
					if !errors.Is(closeErr, ErrAdmissionStageRecovery) || !errors.Is(handle.Err(), ErrAdmissionStageRecovery) {
						t.Error("corrupt handle lost its recovery error", closeErr, handle.Err())
					}
				} else if closeErr != nil {
					t.Error(closeErr)
				}
			}()
			if kind == "root" {
				if err = os.MkdirAll(handle.Root(), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "corrupt" {
				handle.err = ErrAdmissionStageRecovery
			}
			if err = s.SavePendingWithTemporaryReservation(handle, reg.ArchiveSessionID, p); !errors.Is(err, ErrAdmissionStageRecovery) {
				t.Fatal("invalid handle permitted write", err)
			}
			if _, err = os.Stat(s.pendingPath(reg.ArchiveSessionID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid handle allocated pending", err)
			}
		})
	}
}
