package nativecodec

import (
	"sort"
	"strings"
)

type keyNameSet struct {
	names  map[string]bool
	capped bool
}

func (s *keyNameSet) add(key string) {
	if s.names == nil {
		s.names = map[string]bool{}
	}
	if s.names[key] {
		return
	}
	if len(s.names) >= maxOmittedKeyNames {
		s.capped = true
		return
	}
	s.names[key] = true
}

func (s *keyNameSet) detail(intro string) string {
	if len(s.names) == 0 {
		return ""
	}
	names := make([]string, 0, len(s.names))
	for key := range s.names {
		names = append(names, key)
	}
	sort.Strings(names)
	detail := intro + strings.Join(names, ", ")
	if s.capped {
		detail += "; further key names omitted"
	}
	return detail
}

func recordTypeAllowed(known map[string]bool, kind string, cursorRole bool) bool {
	return known[kind] || cursorRole
}

const maxOmittedKeyNames = 64

const deniedToolArgumentIntro = "omitted tool argument keys: "

var compactBoundaryIDKeys = map[string]bool{"uuid": true, "parentUuid": true, "logicalParentUuid": true, "sessionId": true}

var conversationRecordTypes = map[string]bool{"user": true, "assistant": true, "system": true, "message": true, "tool_use": true, "tool_result": true, "tool_call": true, "session_meta": true, "turn_context": true, "response_item": true, "event_msg": true, "session": true, "event": true}
