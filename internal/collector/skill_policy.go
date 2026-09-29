package collector

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func (o Options) skillEvidence() config.SkillEvidence {
	if o.SkillEvidence == "" {
		return config.SkillEvidenceBody
	}
	return o.SkillEvidence
}

func pendingSkillMode(value string) config.SkillEvidence {
	if value == "" {
		return config.SkillEvidenceBody
	}
	return config.SkillEvidence(value)
}

// limitSkillEvidence also removes skill evidence retained in an older local
// candidate. Otherwise a policy change would re-upload cached snapshots.
func limitSkillEvidence(in []archive.SupplementalEvidence, mode config.SkillEvidence) []archive.SupplementalEvidence {
	if mode == config.SkillEvidenceBody {
		return in
	}
	out := make([]archive.SupplementalEvidence, 0, len(in))
	for _, item := range in {
		if item.Kind == archive.EvidenceKindSkillSnapshot {
			continue
		}
		if mode == config.SkillEvidenceNone && item.Kind == archive.EvidenceKindSkillInventory {
			continue
		}
		out = append(out, item)
	}
	return out
}

func sourceEvidenceWithinPolicy(in []archive.SupplementalEvidence, mode config.SkillEvidence) bool {
	for _, item := range in {
		if item.Kind == archive.EvidenceKindSkillSnapshot && mode != config.SkillEvidenceBody {
			return false
		}
		if item.Kind == archive.EvidenceKindSkillInventory && mode == config.SkillEvidenceNone {
			return false
		}
	}
	return true
}
