package nativecodec

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"math"
	"strconv"
	"strings"
)

// Filter 16 keeps explicit and generated native session names and the pull request it is
// linked to. Claude Code writes them as two record types of their own:
//
//	{"type":"custom-title","customTitle":…,"sessionId":…}
//	{"type":"ai-title","aiTitle":…,"sessionId":…}
//	{"type":"pr-link","prNumber":…,"prRepository":…,"prUrl":…,"sessionId":…,"timestamp":…}
//
// Neither is admitted through allowedKeys, which would admit the same key
// names on every record. Each is rebuilt from typed values, as a
// compact_boundary record is, and only then sanitized.
//
// claudeLabelKind is the type of such a record.
type claudeLabelKind string

// Typed native label records are rebuilt through claudeLabelRecord.
const (
	claudeCustomTitleType claudeLabelKind = "custom-title"
	claudeAITitleType     claudeLabelKind = "ai-title"
	claudePRLinkType      claudeLabelKind = "pr-link"
)

// claudeLabelKeys are the keys a rebuilt custom-title or pr-link record
// carries beyond the ones every record may: they are admitted for that record
// only, and only as the flat values claudeLabelRecord checked.
var claudeLabelKeys = map[string]bool{
	"aititle": true, "customtitle": true, "prnumber": true, "prrepository": true, "prurl": true,
}

// claudeLabelPayloadKeys are the keys each record type keeps besides the
// identity keys; claudeLabelRecord checks their values below.
var claudeLabelPayloadKeys = map[claudeLabelKind]map[string]bool{
	claudeCustomTitleType: {"customTitle": true},
	claudeAITitleType:     {"aiTitle": true},
	claudePRLinkType:      {"prNumber": true, "prRepository": true, "prUrl": true},
}

// claudeLabelIdentityKeys are kept on a rebuilt record when they are strings,
// and then pass the ordinary value rules.
var claudeLabelIdentityKeys = map[string]bool{"sessionId": true, "timestamp": true}

// isClaudeLabelType reports whether a Claude Code record type is one filter 13
// keeps through claudeLabelRecord.
func isClaudeLabelType(kind string) bool {
	switch claudeLabelKind(kind) {
	case claudeCustomTitleType, claudeAITitleType, claudePRLinkType:
		return true
	}
	return false
}

// claudeLabelRecord rebuilds a custom-title or pr-link record from what the
// filter keeps of it, reporting the name of every key it leaves out through
// omit. It returns false when the record has nothing it may keep or is out of
// shape:
//
//   - custom-title keeps customTitle when it is a non-empty string. The text
//     goes through the same redaction as a prompt.
//   - pr-link keeps prNumber, as the integer it is whether Claude Code wrote
//     it as a string ("213") or a number, and prRepository, both in the shape
//     git_activity requires. prUrl is kept only when it is exactly the GitHub
//     URL those two make; any other URL (another host, a query string) is
//     dropped and the rest of the record is kept. A pr-link whose repository
//     or number is missing or out of shape is dropped whole.
//
// These records keep type, string sessionId/timestamp and boolean isSidechain.
func claudeLabelRecord(raw map[string]any, omit func(string)) (map[string]any, bool) {
	rawKind, _ := raw["type"].(string)
	kind := claudeLabelKind(rawKind)
	out := map[string]any{"type": rawKind}
	for _, key := range sortedKeys(raw) {
		value := raw[key]
		switch {
		case key == "type":
		case key == "isSidechain":
			if sidechain, ok := value.(bool); ok {
				out[key] = sidechain
			} else {
				omit(key)
			}
		case claudeLabelIdentityKeys[key]:
			if text, ok := value.(string); ok {
				out[key] = text
			} else {
				omit(key)
			}
		case claudeLabelPayloadKeys[kind][key]:
		default:
			omit(key)
		}
	}
	if kind == claudeCustomTitleType || kind == claudeAITitleType {
		key := "customTitle"
		if kind == claudeAITitleType {
			key = "aiTitle"
		}
		title, ok := raw[key].(string)
		if !ok || strings.TrimSpace(title) == "" {
			return nil, false
		}
		out[key] = title
		return out, true
	}
	number, ok := claudePRNumber(raw["prNumber"])
	repository, _ := raw["prRepository"].(string)
	owner, name, repositoryOK := splitRepository(repository)
	if !ok || !repositoryOK {
		return nil, false
	}
	out["prNumber"] = float64(number)
	out["prRepository"] = repository
	if link, present := raw["prUrl"]; present {
		if link == claudePRURL(owner, name, number) {
			out["prUrl"] = link
		} else {
			omit("prUrl")
		}
	}
	return out, true
}

// claudePRNumber reads a pr-link's prNumber, written as a string of digits or
// as a whole JSON number.
func claudePRNumber(value any) (int, bool) {
	switch v := value.(type) {
	case string:
		return parsePRNumber(v)
	case float64:
		if v != math.Trunc(v) || v < 1 || v > maxPRNumber {
			return 0, false
		}
		return int(v), true
	}
	return 0, false
}

// claudePRURL is the GitHub URL of a pull request, the only prUrl filter 13
// keeps.
func claudePRURL(owner, name string, number int) string {
	return "https://github.com/" + owner + "/" + name + "/pull/" + strconv.Itoa(number)
}

// claudeLabelSurvived reports whether the sanitized record still holds what
// made it worth keeping: a title, or the repository and number exactly as
// claudeLabelRecord checked them. The value rules can change a string (a
// credential-shaped one is replaced) or drop it; a pull request whose
// repository was rewritten no longer says which one it is. prUrl is dropped
// when it changed, and its name reported through omit, as it is when
// claudeLabelRecord drops it.
func claudeLabelSurvived(label, safe map[string]any, omit func(string)) bool {
	if label["type"] == string(claudeCustomTitleType) || label["type"] == string(claudeAITitleType) {
		key := "customTitle"
		if label["type"] == string(claudeAITitleType) {
			key = "aiTitle"
		}
		title, ok := safe[key].(string)
		return ok && strings.TrimSpace(title) != ""
	}
	if safe["prRepository"] != label["prRepository"] || safe["prNumber"] != label["prNumber"] {
		return false
	}
	if link, present := label["prUrl"]; present && safe["prUrl"] != link {
		delete(safe, "prUrl")
		omit("prUrl")
	}
	return true
}

// filterClaudeLabel filters one custom-title or pr-link record and returns
// its sanitized map, or nil when the record is dropped. A dropped record is
// reported in an unsupported_value_omitted gap, which carries no content.
func filterClaudeLabel(raw map[string]any, lineNo int, addGap func(string, int, string), omit func(string)) map[string]any {
	label, ok := claudeLabelRecord(raw, omit)
	if !ok {
		addGap("unsupported_value_omitted", lineNo, "record omitted")
		return nil
	}
	state := archive.PrivacyState{Record: lineNo, AddGap: addGap, ExtraAllowed: claudeLabelKeys, OmittedKey: omit}
	safe, keep := sanitizeObject(label, &state)
	if !keep || !claudeLabelSurvived(label, safe, omit) {
		addGap("unsupported_value_omitted", lineNo, "record omitted")
		return nil
	}
	return safe
}
