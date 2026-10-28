package muse

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// refusal names the native record family a strict decode refused at line.
// Only failures of the closed native format are named; budget, cancellation
// and I/O errors pass through unchanged.
func refusal(raw []byte, line int64, err error) error {
	if !errors.Is(err, errFormat) {
		return err
	}
	var native struct {
		Frame       *string `json:"retained_frame"`
		PayloadType string  `json:"payload_type"`
		Payload     struct {
			Kind  any `json:"kind"`
			Event struct {
				Kind any `json:"kind"`
			} `json:"event"`
			Record struct {
				Kind any `json:"kind"`
			} `json:"record"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &native) != nil {
		return agentapi.NewNativeRefusalError(agentapi.RefusalUnsupportedRecord, "", line, err)
	}
	if native.Frame != nil {
		return agentapi.NewNativeRefusalError(agentapi.RefusalUnsupportedRecord, *native.Frame, line, err)
	}
	parts := []string{native.PayloadType}
	for _, kind := range []any{native.Payload.Kind, native.Payload.Event.Kind, native.Payload.Record.Kind} {
		if s, ok := kind.(string); ok && s != "" && !strings.Contains(native.PayloadType, s) {
			parts = append(parts, s)
		}
	}
	family := strings.Join(parts, "/")
	if native.PayloadType == "" {
		family = ""
	}
	if line == 1 && family != "" {
		// The session layout's first line is always the permission frame. A file
		// starting with an ordinary record is a native side log, not a session.
		return agentapi.NewNativeRefusalError(agentapi.RefusalNoSessionStart, family, line, err)
	}
	return agentapi.NewNativeRefusalError(agentapi.RefusalUnsupportedRecord, family, line, err)
}
