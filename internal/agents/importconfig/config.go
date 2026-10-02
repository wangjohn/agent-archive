// Package importconfig supplies pure native historical inspection mechanics.
// Each integration declares its own record vocabulary and identity/start rules.
package importconfig

import (
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"slices"
	"strings"
)

// Provider is a native declaration, consumed through actual backfill injection.
type Provider struct {
	FileCreatedStart         bool
	RequireSessionMembership bool
	PreferHeaderStart        bool
	NativeStart              bool
	ConversationTypes        []string
}

func (p Provider) ImportPolicy(agentapi.SourceRef) agentapi.ImportPolicy {
	if p.FileCreatedStart {
		return agentapi.ImportPolicy{Start: agentapi.ImportFileCreatedStart}
	}
	return agentapi.ImportPolicy{Start: agentapi.ImportNativeStart}
}

func (p Provider) InspectImport(ctx context.Context, r agentapi.ImportInspectionRequest) (out agentapi.ImportInspection, err error) {
	for _, text := range r.Filtered.Text {
		if strings.TrimSpace(text) != "" {
			out.Conversation = true
			break
		}
	}
	for _, record := range r.Filtered.Records {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		var value struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if json.Unmarshal(record, &value) == nil && (slices.Contains(p.ConversationTypes, value.Type) || value.Role != "") {
			out.Conversation = true
			break
		}
	}
	if p.RequireSessionMembership && (len(r.Filtered.SessionIDs) > 0 || out.Conversation) && !slices.Contains(r.Filtered.SessionIDs, r.Session.NativeID) {
		out.IdentityMismatch = true
	}
	if p.PreferHeaderStart && !r.Header.StartedAt.IsZero() {
		out.StartedAt = r.Header.StartedAt
	} else if p.NativeStart {
		out.StartedAt = r.Filtered.NativeStartAt
	}
	return out, ctx.Err()
}
