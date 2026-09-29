package collector

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/evidence"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestSkillPolicyLimitsPendingAndUploadedBytes(t *testing.T) {
	for _, mode := range []config.SkillEvidence{config.SkillEvidenceNone, config.SkillEvidenceMetadata} {
		t.Run(string(mode), func(t *testing.T) {
			project := t.TempDir()
			skill := filepath.Join(project, ".agents", "skills", "secret", "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(skill), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(skill, []byte("---\nname: secret\n---\nunique-private-skill-body"), 0600); err != nil {
				t.Fatal(err)
			}
			transcript := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript)
			reg := registration(t, transcript)
			reg.ProjectRoot = project
			local := newTestStore(t)
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
			now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
			options := Options{MachineID: "m", Now: func() time.Time { return now }, SkillEvidence: mode,
				SupplementalEvidence: func(_ archive.SessionRegistration, at time.Time) ([]archive.SupplementalEvidence, error) {
					return evidence.ObserveSkills(evidence.SkillOptions{Harness: "codex", ProjectRoot: project, ObservedAt: at, Mode: mode})
				},
			}
			_, _ = Run(context.Background(), local, remote, options)
			pending, found, err := local.LoadPending(reg.ArchiveSessionID)
			if err != nil || !found {
				t.Fatalf("pending: %v %v", found, err)
			}
			check := func(label string, source []byte) {
				t.Helper()
				bundle, err := archive.ReadSourceBundle(bytes.NewReader(source), archive.DecodeOptions{})
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range bundle.SupplementalEvidence {
					if item.Kind == archive.EvidenceKindSkillSnapshot || mode == config.SkillEvidenceNone && item.Kind == archive.EvidenceKindSkillInventory {
						t.Fatalf("%s carried forbidden %s", label, item.Kind)
					}
				}
				if strings.Contains(string(source), "unique-private-skill-body") {
					t.Fatalf("%s leaked body", label)
				}
			}
			check("pending", pending.SourceBytes)
			remote.failMetadata = false
			if _, err := Run(context.Background(), local, remote, options); err != nil {
				t.Fatal(err)
			}
			meta := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
			uploaded, err := remote.Get(context.Background(), meta.SourceBundle.Key)
			if err != nil {
				t.Fatal(err)
			}
			check("uploaded", uploaded)
		})
	}
}

func TestStricterPolicyRebuildsFrozenPendingSource(t *testing.T) {
	project := t.TempDir()
	skill := filepath.Join(project, ".agents", "skills", "sample", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("---\nname: sample\n---\nprivate-skill-body"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript))
	reg.ProjectRoot = project
	local := newTestStore(t)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	mode := config.SkillEvidenceBody
	options := Options{MachineID: "m", Now: func() time.Time { return time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC) }, SkillEvidence: mode,
		SupplementalEvidence: func(_ archive.SessionRegistration, at time.Time) ([]archive.SupplementalEvidence, error) {
			return evidence.ObserveSkills(evidence.SkillOptions{Harness: "codex", ProjectRoot: project, ObservedAt: at, Mode: mode})
		},
	}
	_, _ = Run(context.Background(), local, remote, options)
	old, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatalf("old pending: %v %v", found, err)
	}
	if !strings.Contains(old.Bundle.SupplementalEvidence[len(old.Bundle.SupplementalEvidence)-1].Payload["snapshot"].(string), "private-skill-body") {
		t.Fatal("body setup failed")
	}
	mode = config.SkillEvidenceNone
	options.SkillEvidence = mode
	_, _ = Run(context.Background(), local, remote, options)
	next, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatalf("new pending: %v %v", found, err)
	}
	if next.SourceSHA256 == old.SourceSHA256 {
		t.Fatal("reused broader pending source")
	}
	for _, item := range next.Bundle.SupplementalEvidence {
		if item.Kind == archive.EvidenceKindSkillInventory || item.Kind == archive.EvidenceKindSkillSnapshot {
			t.Fatalf("new pending carried %s", item.Kind)
		}
	}
	remote.failMetadata = false
	if _, err := Run(context.Background(), local, remote, options); err != nil {
		t.Fatal(err)
	}
	meta := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if meta.SourceBundle.Key != next.SourceKey {
		t.Fatal("live metadata did not point to policy-limited source")
	}
}

func TestStricterPolicyReplacesPublishedSourceWithoutTranscriptChange(t *testing.T) {
	project := t.TempDir()
	skill := filepath.Join(project, ".agents", "skills", "sample", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("---\nname: sample\n---\nprivate-skill-body"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript))
	reg.ProjectRoot = project
	local := newTestStore(t)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	mode := config.SkillEvidenceBody
	options := Options{MachineID: "m", Now: func() time.Time { return now }, SkillEvidence: mode,
		SupplementalEvidence: func(_ archive.SessionRegistration, at time.Time) ([]archive.SupplementalEvidence, error) {
			return evidence.ObserveSkills(evidence.SkillOptions{Harness: "codex", ProjectRoot: project, ObservedAt: at, Mode: mode})
		},
	}
	if _, err := Run(context.Background(), local, remote, options); err != nil {
		t.Fatal(err)
	}
	old := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	mode = config.SkillEvidenceNone
	options.SkillEvidence = mode
	now = now.Add(time.Hour)
	if _, err := Run(context.Background(), local, remote, options); err != nil {
		t.Fatal(err)
	}
	next := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if next.SourceBundle.Key == old.SourceBundle.Key {
		t.Fatal("policy downgrade retained old source")
	}
	source, err := remote.Get(context.Background(), next.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.ReadSourceBundle(bytes.NewReader(source), archive.DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range bundle.SupplementalEvidence {
		if item.Kind == archive.EvidenceKindSkillInventory || item.Kind == archive.EvidenceKindSkillSnapshot {
			t.Fatalf("downgraded source carried %s", item.Kind)
		}
	}
}
