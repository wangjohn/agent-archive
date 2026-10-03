package discovery

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// ReadHealth reads only a bounded content-free summary, never source or catalog state.
func ReadHealth(home string) (Health, bool, error) {
	f, e := os.Open(filepath.Join(home, "discovery-health.json"))
	if errors.Is(e, os.ErrNotExist) {
		return Health{}, false, nil
	}
	if e != nil {
		return Health{}, false, e
	}
	defer func() { _ = f.Close() }()
	b, e := io.ReadAll(io.LimitReader(f, maxHealthBytes+1))
	if e != nil {
		return Health{}, false, e
	}
	if len(b) > maxHealthBytes {
		return Health{}, false, errors.New("discovery health is oversized")
	}
	var s healthSummary
	if e = json.Unmarshal(b, &s); e != nil {
		return Health{}, false, e
	}
	if s.Version != 1 || len(s.Health.Outcomes) > 64 || len(s.Health.Errors) > 32 {
		return Health{}, false, errors.New("invalid discovery health version or limits")
	}
	if e = validateHealth(s.Health); e != nil {
		return Health{}, false, e
	}
	return s.Health, true, nil
}
