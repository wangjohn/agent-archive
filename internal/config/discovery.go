package config

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"reflect"
	"slices"
	"strings"
	"time"
)

const discoveryWriterMarker = "+discovery-v2"

const codexWriterMarker = "+codex-scope-floor-v3"

const legacyCodexWriterMarker = "+codex-scope-v3"

// DiscoveryConfig records consent, roots and current per-project generations.
// Retain it even after disablement: old writers must never flatten the history.
type DiscoveryConfig struct {
	ChoiceRecorded bool                     `json:"choice_recorded,omitempty"`
	Enabled        bool                     `json:"enabled"`
	CodexHomes     []string                 `json:"codex_homes"`
	Authorizations []DiscoveryAuthorization `json:"authorizations"`
}

// SetDiscoveryChoice records proposed setup consent without opening authorization
// intervals. ReconcileDiscovery opens intervals only for the committed scope.
func SetDiscoveryChoice(c *Config, enabled bool) {
	d := DiscoveryConfig{}
	if c.Discovery != nil {
		d = *c.Discovery
	}
	d.Enabled = enabled
	d.ChoiceRecorded = true
	c.Discovery = &d
	_ = prepareDiscoveryConfig(c)
}

// SetSkillEvidence changes the user policy while retaining discovery writer protection.
func SetSkillEvidence(c *Config, mode SkillEvidence) {
	c.SkillEvidence = mode
	_ = prepareDiscoveryConfig(c)
}

// DiscoveryAuthorization is a current permission generation for one scope.
type DiscoveryAuthorization struct {
	Generation string `json:"generation"`
	// NativeStartFloor is immutable within a generation, including while paused.
	// Zero identifies an older empty history whose consent boundary is unknown.
	NativeStartFloor time.Time           `json:"native_start_floor,omitzero"`
	Agent            string              `json:"agent"`
	ProjectRoot      string              `json:"project_root"`
	DestinationID    string              `json:"destination_id"`
	Intervals        []DiscoveryInterval `json:"intervals"`
}

// DiscoveryInterval is a half-open unpaused interval; zero end is open.
type DiscoveryInterval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitzero"`
}

func underlyingSkillEvidence(mode SkillEvidence) SkillEvidence {
	return SkillEvidence(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(string(mode), codexWriterMarker), legacyCodexWriterMarker), discoveryWriterMarker))
}

func prepareDiscoveryConfig(c *Config) error {
	if c.SchemaVersion > 3 || (c.SchemaVersion > 2 && c.CodexCapture == nil) {
		return errors.New("configuration requires a newer agent-archive writer")
	}
	if c.Discovery != nil {
		// Migrate only provable retained permission, on a private value copy.
		// Empty old histories have no recoverable floor and remain fail closed.
		d := *c.Discovery
		d.Authorizations = slices.Clone(d.Authorizations)
		for i := range d.Authorizations {
			a := &d.Authorizations[i]
			if a.NativeStartFloor.IsZero() && len(a.Intervals) > 0 {
				a.NativeStartFloor = a.Intervals[0].Start
			}
		}
		c.Discovery = &d
		c.SkillEvidence = SkillEvidence(string(c.EffectiveSkillEvidence()) + discoveryWriterMarker)
		c.SchemaVersion = 2
	}
	if c.CodexCapture != nil {
		p := *c.CodexCapture
		for _, target := range []**DiscoveryAuthorization{&p.Authorization, &p.SourceAuthorization} {
			if *target != nil {
				a := **target
				a.Intervals = slices.Clone(a.Intervals)
				a.NativeStartFloor = discoveryNativeStartFloor(a)
				*target = &a
			}
		}
		c.CodexCapture = &p
		c.SkillEvidence = SkillEvidence(string(c.EffectiveSkillEvidence()) + codexWriterMarker)
		c.SchemaVersion = 3
	}
	return validateDiscoveryConfig(*c)
}

func validateDiscoveryConfig(c Config) error {
	if c.CodexCapture != nil {
		if c.SchemaVersion != 3 || (!strings.HasSuffix(string(c.SkillEvidence), codexWriterMarker) && !strings.HasSuffix(string(c.SkillEvidence), legacyCodexWriterMarker)) {
			return errors.New("codex scope requires protected schema 3")
		}
		if c.CodexCapture.Scope != CodexIncludedProjects && c.CodexCapture.Scope != CodexAllProjects {
			return errors.New("invalid Codex scope")
		}
		if len(c.CodexCapture.Barriers) > 4096 || len(c.CodexCapture.RuleRoots) > 4096 || len(c.Archive.Projects) > 4096 {
			return errors.New("codex exception history limit exceeded")
		}
		for _, a := range []*DiscoveryAuthorization{c.CodexCapture.Authorization, c.CodexCapture.SourceAuthorization} {
			if err := validateCodexAuthorization(a); err != nil {
				return err
			}
		}
		for _, b := range c.CodexCapture.Barriers {
			if b.Root == "" || b.Since.IsZero() {
				return errors.New("invalid Codex subtree barrier")
			}
		}
		return validateDiscoveryPayload(c.Discovery)
	}
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
	return validateDiscoveryPayload(c.Discovery)
}

func validateCodexAuthorization(a *DiscoveryAuthorization) error {
	if a == nil {
		return nil
	}
	if a.Generation == "" || a.Agent != "codex" || a.ProjectRoot != "" || a.DestinationID == "" || len(a.Intervals) > 256 {
		return errors.New("invalid Codex authorization")
	}
	var prior time.Time
	for i, v := range a.Intervals {
		if v.Start.IsZero() || (!a.NativeStartFloor.IsZero() && v.Start.Before(a.NativeStartFloor)) || (!v.End.IsZero() && !v.End.After(v.Start)) || (i > 0 && (prior.IsZero() || v.Start.Before(prior))) {
			return errors.New("invalid Codex authorization intervals")
		}
		prior = v.End
	}
	return nil
}

// ReconcileDiscovery creates new generations when effective scope changes.
// Setup calls it before its journal commits the config and all permissions.
func ReconcileDiscovery(next *Config, previous Config, now time.Time) error {
	nativeConsentStart := now.UTC()
	if next.DestinationSince.After(nativeConsentStart) {
		nativeConsentStart = next.DestinationSince
	}
	working := *next
	if err := reconcileDiscovery(&working, previous, now, nativeConsentStart); err != nil {
		return err
	}
	*next = working
	return nil
}

func reconcileDiscovery(next *Config, previous Config, now, nativeConsentStart time.Time) error {
	if err := ReconcileCodexCapture(next, previous, now); err != nil {
		return err
	}
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
			if previous.Discovery != nil && previous.Discovery.Enabled && slices.Contains(previous.Harnesses, "codex") && previous.Archive.Enabled && sameDiscoveryProjectScope(p.Root, previous, *next) && slices.Equal(previous.Discovery.CodexHomes, d.CodexHomes) {
				for _, a := range previous.Discovery.Authorizations {
					if a.ProjectRoot == p.Root && a.DestinationID == next.DestinationID() && (!a.NativeStartFloor.IsZero() || len(a.Intervals) > 0) {
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
				start := nativeConsentStart
				if p.ActivatedAt.After(start) {
					start = p.ActivatedAt
				}
				var intervals []DiscoveryInterval
				if !next.Paused {
					intervals = []DiscoveryInterval{{Start: start}}
				}
				a := DiscoveryAuthorization{Generation: id, NativeStartFloor: start, Agent: "codex", ProjectRoot: p.Root, DestinationID: next.DestinationID(), Intervals: intervals}
				kept = &a
			}
			d.Authorizations = append(d.Authorizations, *kept)
		}
	}
	next.Discovery = &d
	return prepareDiscoveryConfig(next)
}

// transitionDiscoveryPause preflights every authority before changing any history.
func transitionDiscoveryPause(c *Config, paused bool, now time.Time) error {
	if c.Paused == paused {
		return nil
	}
	var authorities []*DiscoveryAuthorization
	if c.CodexCapture != nil {
		authorities = append(authorities, c.CodexCapture.Authorization, c.CodexCapture.SourceAuthorization)
	}
	if c.Discovery != nil {
		for i := range c.Discovery.Authorizations {
			authorities = append(authorities, &c.Discovery.Authorizations[i])
		}
	}
	for _, a := range authorities {
		if a != nil {
			if err := checkIntervalTransition(*a, paused, now); err != nil {
				return err
			}
		}
	}
	for _, a := range authorities {
		if a != nil {
			if err := transitionIntervals(a, paused, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkIntervalTransition(a DiscoveryAuthorization, paused bool, now time.Time) error {
	if !paused {
		floor := discoveryNativeStartFloor(a)
		if floor.IsZero() {
			return errors.New("discovery consent boundary is unavailable; run setup to renew authorization before resuming")
		}
		if now.Before(floor) {
			return errors.New("clock precedes discovery consent boundary; correct the clock before pausing or resuming")
		}
	}
	if n := len(a.Intervals); n > 0 {
		last := a.Intervals[n-1]
		if (paused && last.End.IsZero() && !now.After(last.Start)) || (!paused && (last.End.IsZero() || now.Before(last.End))) {
			return errors.New("clock precedes discovery consent boundary; correct the clock before pausing or resuming")
		}
	}
	return nil
}

func transitionIntervals(a *DiscoveryAuthorization, paused bool, now time.Time) error {
	if err := checkIntervalTransition(*a, paused, now); err != nil {
		return err
	}
	a.Intervals = slices.Clone(a.Intervals)
	if paused {
		if n := len(a.Intervals); n > 0 && a.Intervals[n-1].End.IsZero() {
			a.Intervals[n-1].End = now
		}
	} else {
		if len(a.Intervals) >= 256 {
			a.Intervals = a.Intervals[1:]
		}
		a.Intervals = append(a.Intervals, DiscoveryInterval{Start: now})
	}
	return nil
}

// DiscoveryGeneration authorizes native start evidence under the current
// agent/project/destination generation. Scan time is never a start boundary.
func (c Config) DiscoveryGeneration(agent, root string, started, now time.Time) (string, bool) {
	if c.Discovery == nil || !c.Discovery.Enabled || c.Paused || !c.Archive.Enabled || !slices.Contains(c.Harnesses, agent) || started.IsZero() || started.After(now.Add(2*time.Minute)) {
		return "", false
	}
	if agent == "codex" && c.EffectiveCodexCaptureScope() == CodexAllProjects {
		return c.CodexDiscoveryGeneration(root, root, started, now)
	}
	for _, a := range c.Discovery.Authorizations {
		if a.Agent != agent || a.ProjectRoot != root || a.DestinationID != c.DestinationID() {
			continue
		}
		floor := discoveryNativeStartFloor(a)
		if floor.IsZero() || started.Before(floor) {
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

// Legacy retained history can only narrow permission to its earliest surviving
// interval. Activation or observation times cannot recover an empty history.
func discoveryNativeStartFloor(a DiscoveryAuthorization) time.Time {
	if !a.NativeStartFloor.IsZero() {
		return a.NativeStartFloor
	}
	if len(a.Intervals) > 0 {
		return a.Intervals[0].Start
	}
	return time.Time{}
}

func sameDiscoveryProjectScope(root string, previous, next Config) bool {
	within := func(c Config) []archive.ProjectActivation {
		var relevant []archive.ProjectActivation
		for _, p := range c.Archive.Projects {
			if local.PathWithin(p.Root, root) || local.PathWithin(root, p.Root) {
				relevant = append(relevant, p)
			}
		}
		slices.SortFunc(relevant, func(a, b archive.ProjectActivation) int { return strings.Compare(a.Root, b.Root) })
		return relevant
	}
	return reflect.DeepEqual(within(previous), within(next))
}

// ProtectIdentityWriter preserves policy while making older writers refuse
// namespaced state. Callers hold hooks.lock after checking setup's journal.
// Disablement is explicit: this never grants discovery consent or intervals.
func ProtectIdentityWriter(home string) error {
	c, found, fenced, err := loadConfig(home)
	if err != nil || !found {
		return err
	}
	if (c.Discovery != nil || c.CodexCapture != nil) && fenced {
		return nil
	}
	if c.Discovery == nil {
		c.Discovery = &DiscoveryConfig{}
	}
	return Save(home, c)
}

func validateDiscoveryPayload(d *DiscoveryConfig) error {
	if d == nil {
		return nil
	}
	if len(d.Authorizations) > 4096 || len(d.CodexHomes) > 16 {
		return errors.New("discovery authorization limits exceeded")
	}
	for _, a := range d.Authorizations {
		if a.Generation == "" || a.Agent != "codex" || a.ProjectRoot == "" || a.DestinationID == "" || len(a.Intervals) > 256 {
			return errors.New("invalid discovery authorization")
		}
		var prior time.Time
		for i, v := range a.Intervals {
			if v.Start.IsZero() || (!a.NativeStartFloor.IsZero() && v.Start.Before(a.NativeStartFloor)) || (!v.End.IsZero() && !v.End.After(v.Start)) || (i > 0 && (prior.IsZero() || v.Start.Before(prior))) {
				return errors.New("invalid discovery intervals")
			}
			prior = v.End
		}
	}
	return nil
}
