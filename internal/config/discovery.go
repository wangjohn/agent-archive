package config

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/local"
	"reflect"
	"slices"
	"strings"
	"time"
)

const discoveryWriterMarker = "+discovery-v2"

// DiscoveryConfig records consent, roots and current per-project generations.
// Retain it even after disablement: old writers must never flatten the history.
type DiscoveryConfig struct {
	Enabled        bool                     `json:"enabled"`
	CodexHomes     []string                 `json:"codex_homes"`
	Authorizations []DiscoveryAuthorization `json:"authorizations"`
}

// DiscoveryAuthorization is a current permission generation for one scope.
type DiscoveryAuthorization struct {
	Generation    string              `json:"generation"`
	Agent         string              `json:"agent"`
	ProjectRoot   string              `json:"project_root"`
	DestinationID string              `json:"destination_id"`
	Intervals     []DiscoveryInterval `json:"intervals"`
}

// DiscoveryInterval is a half-open unpaused interval; zero end is open.
type DiscoveryInterval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitzero"`
}

func underlyingSkillEvidence(mode SkillEvidence) SkillEvidence {
	return SkillEvidence(strings.TrimSuffix(string(mode), discoveryWriterMarker))
}
func prepareDiscoveryConfig(c *Config) error {
	if c.Discovery != nil {
		c.SkillEvidence = SkillEvidence(string(c.EffectiveSkillEvidence()) + discoveryWriterMarker)
		c.SchemaVersion = 2
	}
	return validateDiscoveryConfig(*c)
}
func validateDiscoveryConfig(c Config) error {
	marked := strings.HasSuffix(string(c.SkillEvidence), discoveryWriterMarker)
	if marked != (c.Discovery != nil) {
		return errors.New("discovery writer compatibility marker and authorization must be kept together")
	}
	if c.SchemaVersion > 2 {
		return errors.New("configuration requires a newer agent-archive writer")
	}
	if c.Discovery == nil {
		return nil
	}
	if c.SchemaVersion != 2 {
		return errors.New("discovery requires configuration schema 2")
	}
	if len(c.Discovery.Authorizations) > 4096 || len(c.Discovery.CodexHomes) > 16 {
		return errors.New("discovery authorization limits exceeded")
	}
	for _, a := range c.Discovery.Authorizations {
		if a.Generation == "" || a.Agent != "codex" || a.ProjectRoot == "" || a.DestinationID == "" || len(a.Intervals) > 256 {
			return errors.New("invalid discovery authorization")
		}
		var prior time.Time
		for i, v := range a.Intervals {
			if v.Start.IsZero() || (!v.End.IsZero() && !v.End.After(v.Start)) || (i > 0 && (prior.IsZero() || v.Start.Before(prior))) {
				return errors.New("invalid discovery intervals")
			}
			prior = v.End
		}
	}
	return nil
}

// ReconcileDiscovery creates new generations when effective scope changes.
// Setup calls it before its journal commits the config and all permissions.
func ReconcileDiscovery(next *Config, previous Config, now time.Time) error {
	if next.Discovery == nil {
		next.Discovery = previous.Discovery
	}
	if next.Discovery == nil {
		return nil
	}
	d := *next.Discovery
	d.CodexHomes = slices.Clone(d.CodexHomes)
	d.Authorizations = nil
	if d.Enabled && slices.Contains(next.Harnesses, "codex") && next.Archive.Enabled {
		for _, p := range next.Archive.Projects {
			if !p.Included {
				continue
			}
			var kept *DiscoveryAuthorization
			if previous.Discovery != nil && previous.Discovery.Enabled && slices.Contains(previous.Harnesses, "codex") && previous.Archive.Enabled && reflect.DeepEqual(previous.Archive.Projects, next.Archive.Projects) && slices.Equal(previous.Discovery.CodexHomes, d.CodexHomes) {
				for _, a := range previous.Discovery.Authorizations {
					if a.ProjectRoot == p.Root && a.DestinationID == next.DestinationID() {
						a.Intervals = slices.Clone(a.Intervals)
						kept = &a
						break
					}
				}
			}
			if kept == nil {
				id, err := local.ID()
				if err != nil {
					return err
				}
				start := now.UTC()
				if p.ActivatedAt.After(start) {
					start = p.ActivatedAt
				}
				if next.DestinationSince.After(start) {
					start = next.DestinationSince
				}
				a := DiscoveryAuthorization{Generation: id, Agent: "codex", ProjectRoot: p.Root, DestinationID: next.DestinationID()}
				if !next.Paused {
					a.Intervals = []DiscoveryInterval{{Start: start}}
				}
				kept = &a
			}
			d.Authorizations = append(d.Authorizations, *kept)
		}
	}
	next.Discovery = &d
	return prepareDiscoveryConfig(next)
}
func transitionDiscoveryPause(c *Config, paused bool, now time.Time) {
	if c.Discovery == nil || c.Paused == paused {
		return
	}
	for i := range c.Discovery.Authorizations {
		a := &c.Discovery.Authorizations[i]
		if paused {
			if n := len(a.Intervals); n > 0 && a.Intervals[n-1].End.IsZero() {
				if now.After(a.Intervals[n-1].Start) {
					a.Intervals[n-1].End = now
				} else {
					a.Intervals = a.Intervals[:n-1]
				}
			}
		} else {
			// Expired interval history fails closed, never widens prior permission.
			if len(a.Intervals) >= 256 {
				a.Intervals = a.Intervals[1:]
			}
			a.Intervals = append(a.Intervals, DiscoveryInterval{Start: now})
		}
	}
}

// DiscoveryGeneration authorizes native start evidence under the current
// agent/project/destination generation. Scan time is never a start boundary.
func (c Config) DiscoveryGeneration(agent, root string, started, now time.Time) (string, bool) {
	if c.Discovery == nil || !c.Discovery.Enabled || c.Paused || !c.Archive.Enabled || !slices.Contains(c.Harnesses, agent) || started.IsZero() || started.After(now.Add(2*time.Minute)) {
		return "", false
	}
	for _, a := range c.Discovery.Authorizations {
		if a.Agent != agent || a.ProjectRoot != root || a.DestinationID != c.DestinationID() {
			continue
		}
		for _, v := range a.Intervals {
			if !started.Before(v.Start) && (v.End.IsZero() || started.Before(v.End)) {
				return a.Generation, true
			}
		}
	}
	return "", false
}
