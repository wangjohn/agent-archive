package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
)

// One session can record several capture gaps. The app line counts sessions
// with a gap, not gap entries; the detail line reports both.
func TestStatusCountsSessionsWithCaptureGapsNotGapEntries(t *testing.T) {
	now := time.Now().UTC()
	env, home, _, _ := publishedThroughSync(t, now)
	reg, _ := onlyRegistration(t, home)
	store, err := collector.NewLocalStore(home)
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
	if err := store.SaveBlocked(reg.ArchiveSessionID, bundle, publishedAt, collector.BlockedReasonRecordTooLarge); err != nil {
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

// The storage line leaves out an empty prefix, and "Access checked" reports
// the collector's later confirmation of the same destination rather than
// "never" beside a verified Authentication line.
func TestStatusStorageLineAndAccessCheckedFollowStorageHealth(t *testing.T) {
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

	// Health for another configuration is not evidence for this one.
	write(storageHealth{ConfigurationID: "other", State: "verified", CheckedAt: checked, Context: "background_collector"})
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if !view.StorageVerifiedAt.IsZero() {
		t.Fatalf("access checked from another configuration's health: %v", view.StorageVerifiedAt)
	}

	// A failed check is not a confirmation either.
	write(storageHealth{ConfigurationID: configurationID(cfg), State: "authentication_failed", CheckedAt: checked, Context: "background_collector"})
	if view, err = readStatus(env); err != nil || !view.StorageVerifiedAt.IsZero() {
		t.Fatalf("access checked from a failed check: %v %v", view.StorageVerifiedAt, err)
	}

	write(storageHealth{ConfigurationID: configurationID(cfg), State: "verified", CheckedAt: checked, Context: "background_collector"})
	var out strings.Builder
	if code := runStatusCommand(nil, &out, os.Stderr, env); code != 0 {
		t.Fatalf("status exit=%d output=%s", code, out.String())
	}
	text := out.String()
	if !strings.Contains(text, "Storage:       s3 / bucket\n") {
		t.Fatalf("storage line:\n%s", text)
	}
	if strings.Contains(text, "Access checked: never") || !strings.Contains(text, "Access checked: "+formatTimeOrNever(checked)) {
		t.Fatalf("access checked line:\n%s", text)
	}

	// A later setup verification still wins over older collector health.
	cfg.StorageVerifiedAt = checked.Add(30 * time.Second)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if view, err = readStatus(env); err != nil || !view.StorageVerifiedAt.Equal(cfg.StorageVerifiedAt) {
		t.Fatalf("access checked = %v, want setup's %v (%v)", view.StorageVerifiedAt, cfg.StorageVerifiedAt, err)
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
