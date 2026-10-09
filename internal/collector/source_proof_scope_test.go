package collector

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"testing"
)

func TestRelatedOwnerProofUpgradePreservesOrdinaryScope(t *testing.T) {
	t.Parallel()
	ordinary := archive.SessionRegistration{Harness: archive.Harness{Name: "codex"}}
	if sourceSetVersion(ordinary) != 2 {
		t.Fatal("unbound ordinary owner lost proof2")
	}
	ordinary.CodexBinding = &archive.CodexSourceBinding{NativeThreadID: "native", PhysicalRolloutID: "native"}
	if sourceSetVersion(ordinary) != 2 {
		t.Fatal("ordinary same-thread binding lost proof2")
	}
	zero := uint64(0)
	ordinary.CodexBinding.OwnStart = &zero
	if sourceSetVersion(ordinary) != 3 {
		t.Fatal("explicit zero own boundary reused old proof2")
	}
	ordinary.CodexBinding.OwnStart = nil
	ordinary.CodexBinding.PhysicalRolloutID = "revision"
	if sourceSetVersion(ordinary) != 2 {
		t.Fatal("ordinary physical revision lost proof2")
	}
	ordinary.CodexBinding.PhysicalRolloutID = "native"
	ordinary.CodexBinding.Child = true
	if sourceSetVersion(ordinary) != 3 {
		t.Fatal("child without boundary reused old proof2")
	}
	ordinary.Harness.Name = "claude"
	if sourceSetVersion(ordinary) != 0 {
		t.Fatal("Codex proof scope affected another harness")
	}
}
