package claude

import "github.com/wangjohn/agent-archive/internal/agentapi"

// CaptureEvidence declares evidence strength independently of implemented ports and health.
func (CapabilityEvidence) CaptureEvidence() agentapi.CaptureCapabilities {
	documented := func(evidence string) agentapi.CapabilityEvidence {
		return agentapi.CapabilityEvidence{State: agentapi.CapabilityDocumented, Evidence: evidence}
	}
	unavailable := func(evidence, next string) agentapi.CapabilityEvidence {
		return agentapi.CapabilityEvidence{State: agentapi.CapabilityUnavailable, Evidence: evidence, NextAction: next}
	}
	profile := agentapi.CaptureCapabilities{
		SkillEvidence:   unavailable("No supported native eligibility/use contract has been verified.", "Treat eligibility comparisons as unavailable."),
		SubagentLinkage: unavailable("A lifecycle event alone does not provide a verified child transcript and parent link.", "Validate child identity, parent identity, and transcript path for the installed version."),
		AdapterFixtures: documented("Synthetic fixtures exercise the bounded adapter; they do not prove an installed version."),
	}

	profile.SubagentLinkage = agentapi.CapabilityEvidence{State: agentapi.CapabilityFixtureValidated, Evidence: "Documented SubagentStop identity/path plus synthetic JSONL ownership and native timestamp fixtures. Actual installed-version capture is unverified.", NextAction: "Run a synthetic parent/child capture and read-back for the installed version."}
	profile.FreshStart = documented("SessionStart.source distinguishes startup/clear from resume/compact.")
	profile.Transcript = documented("Hooks provide transcript_path to the native JSONL transcript.")
	profile.Lifecycle = documented("SessionStart, Stop, SessionEnd, and SubagentStop are documented.")

	return profile
}

// CapabilityEvidence holds documented and fixture evidence without host observations.
type CapabilityEvidence struct{}
