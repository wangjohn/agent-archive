package state

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestForgetIdleSessionKeepsASessionThatGainedWork(t *testing.T) {
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name         string
		deferForWork bool
		forgotten    bool
	}{
		{"publishable: the request defers it", true, false},
		{"not publishable: its work would never be done", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := newTestStore(t)
			reg := registration(t)
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			if err := local.SaveRequest(reg.ArchiveSessionID, "stop", at); err != nil {
				t.Fatal(err)
			}
			forgotten, err := local.ForgetIdleSession(reg.ArchiveSessionID, reg.NativeSessionID, tc.deferForWork, nil)
			if err != nil || forgotten != tc.forgotten {
				t.Fatalf("forgotten=%t err=%v, want %t", forgotten, err, tc.forgotten)
			}
			_, registered, _ := local.LoadRegistration(reg.ArchiveSessionID)
			_, requested, _ := local.LoadRequest(reg.ArchiveSessionID)
			if registered == tc.forgotten || requested == tc.forgotten {
				t.Fatalf("registered=%t requested=%t after forgotten=%t", registered, requested, tc.forgotten)
			}
		})
	}
}

func TestForgetIdleSessionKeepsASessionWithAPendingPublication(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := local.SavePending(reg.ArchiveSessionID, PendingPublication{SourceKey: "k", MetadataKey: "m", SourceSHA256: "s", SourceBytes: []byte{1}, MetadataBytes: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if forgotten, err := local.ForgetIdleSession(reg.ArchiveSessionID, reg.NativeSessionID, true, nil); err != nil || forgotten {
		t.Fatalf("forgotten=%t err=%v", forgotten, err)
	}
}

// A hook that looked the registration up before retention forgot the session
// must not leave a request behind for it: nothing would ever read it, and it
// would count as pending forever.
func TestSaveRequestForAForgottenSessionLeavesNoOrphan(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if forgotten, err := local.ForgetIdleSession(reg.ArchiveSessionID, reg.NativeSessionID, true, nil); err != nil || !forgotten {
		t.Fatalf("forgotten=%t err=%v", forgotten, err)
	}
	err := local.SaveRequest(reg.ArchiveSessionID, "stop", time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC))
	if !errors.Is(err, ErrSessionNotRegistered) {
		t.Fatalf("err=%v, want ErrSessionNotRegistered", err)
	}
	if _, err := os.Stat(local.requestPath(reg.ArchiveSessionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an orphan request was written: %v", err)
	}
}
