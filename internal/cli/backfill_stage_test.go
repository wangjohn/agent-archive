package cli

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDurableImportMaterializesEverySupportedSyntheticSource(t *testing.T) {
	f, bucket := newImportFixture(t)
	backdateTranscripts(t, f)
	cfg, _, err := config.Load(f.data)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := backfill.BuildPlan(t.Context(), f.env.backfillEnvironment(f.userHome, cfg), newArchiveState(f.data, cfg), cfg, backfill.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = backfill.ApplyToConfig(&cfg, plan, backfillNow.UTC()); err != nil {
		t.Fatal(err)
	}
	if err = config.Save(f.data, cfg); err != nil {
		t.Fatal(err)
	}
	store := state.OpenReadOnly(f.data)
	result, err := (backfill.Registration{Durable: true, Sources: f.env.agentRegistry(), Home: f.data, Store: store, Batch: firstImport, AdmittedAt: backfillNow.UTC(), DestinationID: cfg.DestinationID(), CursorDatabase: f.env.cursorDatabase()}).Run(plan.Imported())
	if err != nil {
		t.Fatalf("supported synthetic materialization: %v (typed cause: %v)", err, errors.Unwrap(err))
	}
	if len(result.Sessions) != len(plan.Imported()) {
		t.Fatal(result)
	}
	if err = os.RemoveAll(f.userHome); err != nil {
		t.Fatal(err)
	}
	published, err := collector.Run(t.Context(), state.OpenReadOnly(f.data), bucket, collector.Options{
		Sources: f.env.agentRegistry(), Parsers: f.env.agentRegistry(), MachineID: cfg.MachineID, SkillEvidence: cfg.EffectiveSkillEvidence(), Now: func() time.Time { return backfillNow.UTC() },
		AcceptSession: func(reg archive.SessionRegistration) bool { return reg.AdmissionStage != "" },
		RepoKey:       func(string) string { t.Fatal("staged restart reran Git"); return "" },
	})
	if err != nil || len(published.Errors) != 0 || len(published.Published) != len(result.Sessions)+len(result.Subagents) {
		t.Fatal(published, err)
	}
}

func TestDurableImportCursorFileReviewCannotAdmitChangedCreationOrFile(t *testing.T) {
	for _, change := range []string{"creation", "replacement"} {
		t.Run(change, func(t *testing.T) {
			f, _ := newImportFixture(t)
			backdateTranscripts(t, f)
			cfg, _, err := config.Load(f.data)
			if err != nil {
				t.Fatal(err)
			}
			creationChanged := false
			env := f.env.backfillEnvironment(f.userHome, cfg)
			env.FileCreated = func(path string) (time.Time, error) {
				info, err := os.Stat(path)
				if err != nil {
					return time.Time{}, err
				}
				at := info.ModTime()
				if creationChanged {
					at = at.Add(time.Hour)
				}
				return at, err
			}
			plan, err := backfill.BuildPlan(t.Context(), env, newArchiveState(f.data, cfg), cfg, backfill.Filters{Harnesses: []string{"cursor"}})
			if err != nil {
				t.Fatal(err)
			}
			var candidate backfill.Candidate
			for _, c := range plan.Imported() {
				if c.SourceKind == archive.SourceKindFile {
					candidate = c
					break
				}
			}
			if candidate.NativeSessionID == "" {
				t.Fatal("no actual Cursor file export candidate")
			}
			if _, err = backfill.ApplyToConfig(&cfg, plan, backfillNow.UTC()); err != nil {
				t.Fatal(err)
			}
			if err = config.Save(f.data, cfg); err != nil {
				t.Fatal(err)
			}
			if change == "creation" {
				creationChanged = true
			} else {
				raw, err := os.ReadFile(candidate.TranscriptPath)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(candidate.TranscriptPath); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(candidate.TranscriptPath, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := (backfill.Registration{Durable: true, Sources: f.env.agentRegistry(), Home: f.data, Store: state.OpenReadOnly(f.data), Batch: firstImport, AdmittedAt: backfillNow.UTC(), DestinationID: cfg.DestinationID()}).Run([]backfill.Candidate{candidate})
			if err == nil || len(result.Sessions) != 0 {
				t.Fatal(result, err)
			}
		})
	}
}

func TestDurableImportBackgroundQuotaStopRetainsCursorAndResumes(t *testing.T) {
	f, _ := newImportFixture(t)
	orphan := filepath.Join(f.data, "admission-stages", "synthetic-reservation.tmp")
	f.env.backfillCheckpoint = func(step string) error {
		if step != "committed" {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(orphan), 0700); err != nil {
			return err
		}
		file, err := os.Create(orphan)
		if err != nil {
			return err
		}
		return errors.Join(file.Truncate(state.AdmissionStageQuota), file.Close())
	}
	_, errOut, code := f.importRun(t, nil, false, "--yes", "--background")
	if code != 1 || !strings.Contains(errOut, "capacity") {
		t.Fatalf("code%d: %s", code, errOut)
	}
	batch, _ := loadBatch(t, f.data)
	if batch.CompletedAt != nil || batch.StagingStopped != "capacity" || len(batch.Sessions) != 0 || batch.AdmissionCursor != "" {
		t.Fatal(batch)
	}
	if err := os.Remove(orphan); err != nil {
		t.Fatal(err)
	}
	f.env.backfillCheckpoint = nil
	_, errOut, code = f.importRun(t, nil, false, "--yes", "--background")
	if code != 0 {
		t.Fatalf("resume: %s", errOut)
	}
	batch, _ = loadBatch(t, f.data)
	if batch.CompletedAt == nil || len(batch.Sessions) != 12 || batch.AdmissionCursor == "" || batch.StagingStopped != "" {
		t.Fatal(batch)
	}
}
