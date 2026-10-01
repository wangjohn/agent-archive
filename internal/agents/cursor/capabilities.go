package cursor

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

	// Observed on the desktop app 3.21.13: a new chat fires no
	// sessionStart; its first hook is beforeSubmitPrompt with
	// transcript_path null, and afterAgentResponse and stop then name the
	// transcript. A resumed chat's first prompt already names its
	// non-empty transcript. cursor_version is not evidence either way.
	profile.FreshStart = documented("A never-seen chat is registered at its first beforeSubmitPrompt (or sessionStart) when transcript_path is null, absent, or names a missing or empty file; a transcript that already has bytes is a resume and is declined. Observed on Cursor 3.21.13.")
	profile.Transcript = documented("A new chat's first prompt carries transcript_path null; afterAgentResponse and stop name ~/.cursor/projects/<workspace>/agent-transcripts/<id>/<id>.jsonl, which is recorded only when it matches the conversation id. Observed on Cursor 3.21.13.")
	profile.Lifecycle = documented("beforeSubmitPrompt, afterAgentResponse, stop, and sessionEnd fire for a desktop chat (sessionEnd can fire mid-turn); sessionStart, subagentStart, and subagentStop are documented.")

	return profile
}

// CapabilityEvidence holds documented and fixture evidence without host observations.
type CapabilityEvidence struct{}
