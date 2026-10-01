package agentapi

// CapabilityState is how well a capture capability is established.
type CapabilityState string

const (
	CapabilityDocumented       CapabilityState = "documented"
	CapabilityFixtureValidated CapabilityState = "fixture_validated"
	CapabilityUnavailable      CapabilityState = "unavailable"
	CapabilityUnknown          CapabilityState = "unknown"
)

// CapabilityEvidence records declared support strength independently of machine health.
type CapabilityEvidence struct {
	State      CapabilityState `json:"state"`
	Evidence   string          `json:"evidence"`
	NextAction string          `json:"next_action,omitempty"`
}

// CaptureCapabilities keeps each evidence dimension explicit.
type CaptureCapabilities struct {
	FreshStart      CapabilityEvidence `json:"fresh_start"`
	Transcript      CapabilityEvidence `json:"transcript"`
	Lifecycle       CapabilityEvidence `json:"lifecycle"`
	SkillEvidence   CapabilityEvidence `json:"skill_evidence"`
	SubagentLinkage CapabilityEvidence `json:"subagent_linkage"`
	AdapterFixtures CapabilityEvidence `json:"adapter_fixtures"`
}

// CapabilityEvidenceProvider declares support evidence without probing the host.
type CapabilityEvidenceProvider interface{ CaptureEvidence() CaptureCapabilities }

// CapabilityEvidenceLookup projects declared evidence, separately from operation availability.
type CapabilityEvidenceLookup interface {
	LookupCapabilityEvidence(string) (CapabilityEvidenceProvider, bool)
}
