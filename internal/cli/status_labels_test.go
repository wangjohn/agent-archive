package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
)

// One session can record several capture gaps. The app line counts sessions
// with a gap, not gap entries; the detail line reports both.
func TestStatusCountsSessionsWithCaptureGapsNotGapEntries(t *testing.T) {
	now := time.Now().UTC()
	env, home, _, _ := publishedThroughSync(t, now)
	reg, _ := onlyRegistration(t, home)
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	bundle, publishedAt, found, err := store.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatalf("setup: %v", err)
	}
	// Two gaps inside the bundle plus the blocked reason: three gaps, one
	// session.
	bundle.Capture.Gaps = append(bundle.Capture.Gaps,
		archive.CaptureGap{Code: "sensitive_content_redacted", Detail: "test"},
		archive.CaptureGap{Code: "hidden_instruction_omitted", Detail: "test"})
	if err := statetest.SaveBlocked(store, reg.ArchiveSessionID, bundle, publishedAt, state.BlockedReasonRecordTooLarge); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	app := view.Apps[0]
	if app.Sessions != 1 || len(app.CaptureGaps) < 3 || app.SessionsWithCaptureGaps != 1 {
		t.Fatalf("sessions=%d gaps=%d sessions with gaps=%d", app.Sessions, len(app.CaptureGaps), app.SessionsWithCaptureGaps)
	}
	var out strings.Builder
	if code := runStatusCommand(nil, &out, os.Stderr, env); code != 0 {
		t.Fatalf("status exit=%d output=%s", code, out.String())
	}
	text := out.String()
	if !strings.Contains(text, "(1 session(s); 1 with a capture gap)") {
		t.Fatalf("app line counts gap entries as sessions:\n%s", text)
	}
	if want := "Capture gaps: " + strconv.Itoa(len(app.CaptureGaps)) + " recorded across 1 session(s)"; !strings.Contains(text, want) {
		t.Fatalf("missing %q:\n%s", want, text)
	}
}

// The storage line leaves out an empty prefix. storage_verified_at stays
// setup's check; storage_access_confirmed_at, and the text Access line, take
// the collector's later verified health for the same configuration, and
// nothing else.
func TestStatusStorageLineAndAccessConfirmation(t *testing.T) {
	now := time.Now().UTC()
	env, home, _, _ := publishedThroughSync(t, now)
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Storage.Prefix = ""
	cfg.StorageVerifiedAt = time.Time{}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	checked := now.Add(-time.Minute).Truncate(time.Second)
	write := func(h storageHealth) {
		t.Helper()
		if err := local.Write(filepath.Join(home, "storage-health.json"), h); err != nil {
			t.Fatal(err)
		}
	}
	text := func() string {
		t.Helper()
		var out strings.Builder
		if code := runStatusCommand(nil, &out, os.Stderr, env); code != 0 {
			t.Fatalf("status exit=%d output=%s", code, out.String())
		}
		return out.String()
	}

	// None of these is evidence for this configuration: another
	// configuration's health, health without a configuration ID (which the
	// Authentication line still reports as verified), and a failed check.
	for name, h := range map[string]storageHealth{
		"other configuration": {ConfigurationID: "other", State: "verified", CheckedAt: checked, Context: "background_collector"},
		"no configuration":    {ConfigurationID: "", State: "verified", CheckedAt: checked, Context: "background_collector"},
		"failed check":        {ConfigurationID: configurationID(cfg), State: "authentication_failed", CheckedAt: checked, Context: "background_collector"},
	} {
		write(h)
		view, err := readStatus(env)
		if err != nil {
			t.Fatal(err)
		}
		if !view.StorageAccessConfirmedAt.IsZero() || view.StorageAccessConfirmedBy != "" || !view.StorageVerifiedAt.IsZero() {
			t.Fatalf("%s: confirmed=%v by=%q verified=%v", name, view.StorageAccessConfirmedAt, view.StorageAccessConfirmedBy, view.StorageVerifiedAt)
		}
		if out := text(); !strings.Contains(out, "Access:        not confirmed yet\n") {
			t.Fatalf("%s: access line:\n%s", name, out)
		}
	}

	write(storageHealth{ConfigurationID: configurationID(cfg), State: "verified", CheckedAt: checked, Context: "background_collector"})
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if !view.StorageAccessConfirmedAt.Equal(checked) || view.StorageAccessConfirmedBy != "collector" || !view.StorageVerifiedAt.IsZero() {
		t.Fatalf("confirmed=%v by=%q verified=%v", view.StorageAccessConfirmedAt, view.StorageAccessConfirmedBy, view.StorageVerifiedAt)
	}
	out := text()
	if !strings.Contains(out, "Storage:       s3 / bucket\n") {
		t.Fatalf("storage line:\n%s", out)
	}
	if !strings.Contains(out, "Access:        confirmed "+formatTimeOrNever(checked)+" by the collector's last successful storage access\n") {
		t.Fatalf("access line:\n%s", out)
	}

	// A later setup check wins over older collector health, and
	// storage_verified_at keeps meaning setup's check.
	cfg.StorageVerifiedAt = checked.Add(30 * time.Second)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if view, err = readStatus(env); err != nil || !view.StorageVerifiedAt.Equal(cfg.StorageVerifiedAt) || !view.StorageAccessConfirmedAt.Equal(cfg.StorageVerifiedAt) || view.StorageAccessConfirmedBy != "setup" {
		t.Fatalf("verified=%v confirmed=%v by=%q err=%v", view.StorageVerifiedAt, view.StorageAccessConfirmedAt, view.StorageAccessConfirmedBy, err)
	}
	if out := text(); !strings.Contains(out, "by setup's storage check (write, read, list, delete)") {
		t.Fatalf("access line:\n%s", out)
	}
}

func TestStorageLabel(t *testing.T) {
	for _, tc := range []struct {
		cfg  credentials.Config
		want string
	}{
		{credentials.Config{Provider: "s3", Bucket: "b"}, "s3 / b"},
		{credentials.Config{Provider: "r2", Bucket: "b", Prefix: "agent-archive/"}, "r2 / b / agent-archive/"},
	} {
		if got := storageLabel(tc.cfg); got != tc.want {
			t.Errorf("storageLabel(%+v) = %q, want %q", tc.cfg, got, tc.want)
		}
	}
}
