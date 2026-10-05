package codexmeta

import "encoding/json"

// GitInfo is optional, bounded repository evidence. Its shape never invalidates thread identity.
type GitInfo struct {
	RepositoryURL string `json:"repository_url,omitempty"`
}

// UnmarshalJSON ignores unavailable optional evidence independently of identity.
func (g *GitInfo) UnmarshalJSON(b []byte) error {
	g.RepositoryURL = ""
	var fields map[string]json.RawMessage
	valid := json.Unmarshal(b, &fields) == nil
	if !valid {
		fields = nil
	}
	raw := fields["repository_url"]
	if len(raw) > 6*4096+2 {
		return nil
	}
	var value string
	if json.Unmarshal(raw, &value) == nil && len(value) <= 4096 {
		g.RepositoryURL = value
	}
	return nil
}
