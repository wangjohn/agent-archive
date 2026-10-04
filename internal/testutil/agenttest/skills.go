// Package agenttest provides capability-specific integration conformance suites.
package agenttest

import (
	"bytes"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"testing"
)

// Skills checks pure planning and separate health inspection for managed templates.
// Fixtures supply bytes their provider owns; no host file or real home is touched.
func Skills(t *testing.T, p agentapi.SkillProvider, owned []byte) {
	t.Helper()
	file := agentapi.HookFile{Path: "/synthetic/skills/SKILL.md", Mode: 0600}
	absent, err := p.Inspect(agentapi.SkillInspectionRequest{File: file, Content: owned})
	if err != nil || absent.State != agentapi.HookAbsent {
		t.Fatalf("absent: %+v %v", absent, err)
	}
	changes, err := p.Plan(agentapi.SkillPlanRequest{Action: agentapi.SkillInstall, File: file, Content: owned})
	if err != nil || len(changes) != 1 || !bytes.Equal(changes[0].After, owned) || changes[0].Existed {
		t.Fatalf("install: %+v %v", changes, err)
	}
	file.Present = true
	file.Regular = true
	file.Bytes = owned
	healthy, err := p.Inspect(agentapi.SkillInspectionRequest{File: file, Content: owned})
	if err != nil || healthy.State != agentapi.HookOwned || healthy.Stale {
		t.Fatalf("healthy: %+v %v", healthy, err)
	}
	changes, err = p.Plan(agentapi.SkillPlanRequest{Action: agentapi.SkillRefresh, File: file, Content: owned})
	if err != nil || len(changes) != 0 {
		t.Fatalf("idempotent refresh: %+v %v", changes, err)
	}
	newer := append(append([]byte(nil), owned...), []byte("\nnew template")...)
	stale, err := p.Inspect(agentapi.SkillInspectionRequest{File: file, Content: newer})
	if err != nil || !stale.Stale {
		t.Fatalf("stale: %+v %v", stale, err)
	}
	changes, err = p.Plan(agentapi.SkillPlanRequest{Action: agentapi.SkillRemove, File: file})
	if err != nil || len(changes) != 1 || !changes[0].Delete || !bytes.Equal(changes[0].Before, owned) {
		t.Fatalf("remove: %+v %v", changes, err)
	}
	file.Bytes = []byte("foreign skill")
	foreign, err := p.Inspect(agentapi.SkillInspectionRequest{File: file, Content: owned})
	if err != nil || foreign.State != agentapi.HookForeign {
		t.Fatalf("foreign: %+v %v", foreign, err)
	}
	changes, err = p.Plan(agentapi.SkillPlanRequest{Action: agentapi.SkillRemove, File: file})
	if err != nil || len(changes) != 0 {
		t.Fatalf("foreign removal: %+v %v", changes, err)
	}
}
