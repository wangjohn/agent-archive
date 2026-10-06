package codexmeta

import (
	"encoding/json"
	"strings"
	"unicode"
)

// Outcome is a content-free interpretation result, never capture authorization.
type Outcome string

// Outcomes distinguish valid ordinary format, unsupported capture and bad metadata.
const (
	NativeFormat          Outcome = "native_format"
	InvalidIdentity       Outcome = "invalid_identity"
	InvalidRelationship   Outcome = "invalid_relationship"
	InvalidMetadata       Outcome = "invalid_metadata"
	UnsupportedExecution  Outcome = "unsupported_execution"
	UnsupportedHistory    Outcome = "unsupported_history"
	UnsupportedProducer   Outcome = "unsupported_producer"
	ChildHistoryPending   Outcome = "child_history_pending"
	ForkHistoryPending    Outcome = "fork_history_pending"
	RelatedHistoryPending Outcome = "related_history_pending"
)

// CodexHistoryPosition identifies an exclusive prefix of a physical rollout.
// The native wire field is named thread_id, but can differ from its thread ID.
type CodexHistoryPosition struct {
	RolloutID     string `json:"thread_id"`
	EndOrdinal    uint64 `json:"end_ordinal_exclusive"`
	EndByteOffset uint64 `json:"end_byte_offset"`
}

// CodexIdentity separates conversation ownership from physical history links.
// Empty optional IDs remain unknown; ordinal pointers distinguish absent and zero.
// These source facts never grant admission or identify the active rollout.
type CodexIdentity struct {
	ThreadID        string
	RootID          string
	ParentID        string
	ForkID          string
	RolloutID       string
	HistoryMode     HistoryMode `json:"history_mode,omitempty"`
	ForkOrdinal     *uint64
	SubagentOrdinal *uint64
	HistoryBase     *CodexHistoryPosition
	Child           bool
}

// Relationships validates understood metadata shapes and direct contradictions.
// Unknown additive fields are ignored. No dependency files are read here.
func (m CodexMeta) Relationships() (CodexIdentity, Outcome) {
	parent, parentOK := optionalID(m.Parent)
	fork, forkOK := optionalID(m.ForkedFrom)
	forkOrdinal, forkOrdinalOK := optionalOrdinal(m.ForkOrdinal)
	subagentOrdinal, subagentOrdinalOK := optionalOrdinal(m.SubagentOrdinal)
	f := CodexIdentity{ThreadID: m.ID, RootID: m.SessionID, HistoryMode: m.HistoryMode,
		ParentID: parent, ForkID: fork, ForkOrdinal: forkOrdinal, SubagentOrdinal: subagentOrdinal}
	if !parentOK || !forkOK || !forkOrdinalOK || !subagentOrdinalOK {
		return f, InvalidRelationship
	}
	if present(m.HistoryBase) {
		var base struct {
			RolloutID     string  `json:"thread_id"`
			EndOrdinal    *uint64 `json:"end_ordinal_exclusive"`
			EndByteOffset *uint64 `json:"end_byte_offset"`
		}
		if json.Unmarshal(m.HistoryBase, &base) != nil || !uuid.MatchString(base.RolloutID) || base.EndOrdinal == nil || base.EndByteOffset == nil {
			return f, InvalidRelationship
		}
		f.HistoryBase = &CodexHistoryPosition{base.RolloutID, *base.EndOrdinal, *base.EndByteOffset}
	}
	for _, raw := range []json.RawMessage{m.ThreadSource, m.AgentPath, m.AgentRole, m.AgentType, m.AgentNickname} {
		if _, ok := optionalText(raw); !ok {
			return f, InvalidMetadata
		}
	}
	role, _ := optionalText(m.AgentRole)
	oldRole, _ := optionalText(m.AgentType)
	if role != "" && oldRole != "" && role != oldRole {
		return f, InvalidRelationship
	}
	thread, _ := optionalText(m.ThreadSource)
	f.Child = f.ParentID != "" || f.SubagentOrdinal != nil || thread == "subagent" || present(m.AgentPath) || present(m.AgentRole) || present(m.AgentType) || present(m.AgentNickname) || (f.RootID != "" && !strings.EqualFold(f.RootID, f.ThreadID))
	if code := m.relationshipSource(&f); code != "" {
		return f, code
	}

	if f.contradictoryRelationship() {
		return f, InvalidRelationship
	}
	return f, ""
}

func (f CodexIdentity) contradictoryRelationship() bool {
	return f.ParentID != "" && (strings.EqualFold(f.ParentID, f.ThreadID) || strings.EqualFold(f.RootID, f.ThreadID)) || f.ForkID != "" && strings.EqualFold(f.ForkID, f.ThreadID) || f.ForkOrdinal != nil && f.ForkID == ""
}

func (m CodexMeta) relationshipSource(f *CodexIdentity) Outcome {
	if present(m.Source) {
		var source string
		if json.Unmarshal(m.Source, &source) != nil {
			var object map[string]json.RawMessage
			if json.Unmarshal(m.Source, &object) != nil || object == nil {
				return InvalidMetadata
			}
			if sub, exists := object["subagent"]; exists {
				f.Child = true
				childParent, code := codexSubagentParent(sub)
				if code != "" {
					return code
				}
				if f.ParentID != "" && childParent != "" && !strings.EqualFold(f.ParentID, childParent) {
					return InvalidRelationship
				}
				if f.ParentID == "" {
					f.ParentID = childParent
				}
			} else {
				return UnsupportedExecution
			}
		}
	}
	return ""
}

// Identity adds the physical locator to validated relationship facts.
func (m CodexMeta) Identity(path string) (CodexIdentity, Outcome) {
	f, outcome := m.Relationships()
	f.RolloutID = RolloutID(path)
	if !m.ValidateIdentity(path) {
		return f, InvalidIdentity
	}
	if outcome == "" && f.HistoryBase != nil && strings.EqualFold(f.HistoryBase.RolloutID, f.RolloutID) {
		outcome = InvalidRelationship
	}
	return f, outcome
}

func optionalID(raw json.RawMessage) (string, bool) {
	if !present(raw) {
		return "", true
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || !uuid.MatchString(value) {
		return "", false
	}
	return value, true
}

func optionalOrdinal(raw json.RawMessage) (*uint64, bool) {
	if !present(raw) {
		return nil, true
	}
	var value uint64
	if json.Unmarshal(raw, &value) != nil {
		return nil, false
	}
	return &value, true
}

func optionalText(raw json.RawMessage) (string, bool) {
	if !present(raw) {
		return "", true
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || len(value) > 4096 || strings.ContainsFunc(value, unicode.IsControl) {
		return "", false
	}
	return value, true
}

type codexSubagentKind string

const (
	subagentReview  codexSubagentKind = "review"
	subagentCompact codexSubagentKind = "compact"
	subagentMemory  codexSubagentKind = "memory_consolidation"
)

func codexSubagentParent(raw json.RawMessage) (string, Outcome) {
	var kind codexSubagentKind
	if json.Unmarshal(raw, &kind) == nil {
		switch kind {
		case subagentReview, subagentCompact, subagentMemory:
			return "", ""
		default:
			return "", UnsupportedExecution
		}
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return "", InvalidMetadata
	}
	if spawn, ok := object["thread_spawn"]; ok {
		var child struct {
			Parent   string          `json:"parent_thread_id"`
			Depth    *int32          `json:"depth"`
			Path     json.RawMessage `json:"agent_path"`
			Role     json.RawMessage `json:"agent_role"`
			Type     json.RawMessage `json:"agent_type"`
			Nickname json.RawMessage `json:"agent_nickname"`
		}
		if json.Unmarshal(spawn, &child) != nil || !uuid.MatchString(child.Parent) || child.Depth == nil || *child.Depth < 0 {
			return "", InvalidRelationship
		}
		for _, value := range []json.RawMessage{child.Path, child.Role, child.Type, child.Nickname} {
			if _, ok := optionalText(value); !ok {
				return "", InvalidMetadata
			}
		}
		return child.Parent, ""
	}
	if other, ok := object["other"]; ok {
		if _, valid := optionalText(other); valid && present(other) {
			return "", ""
		}
		return "", InvalidMetadata
	}
	return "", UnsupportedExecution
}
