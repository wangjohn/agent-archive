// Package builtin composes immutable integrations without host probes.
package builtin

import (
	"fmt"
	"reflect"
	"slices"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/claude"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/agents/cursor"
)

// Integration binds implemented operations to an identity declaration.
type Integration struct {
	Descriptor agentmeta.Descriptor
	Launcher   agentapi.Launcher
	Runtime    agentapi.RuntimeDetector
	Hooks      agentapi.HookConfigurator
	Decoder    agentapi.HookDecoder
}

// Registry holds validated immutable lookups and operation projections.
type Registry struct {
	catalog     agentmeta.Catalog
	bindings    map[agentmeta.ID]Integration
	supporting  map[agentmeta.Operation][]Integration
	runtime     []Integration
	sessionKeys []string
}

// New binds each identity exactly once and derives operations from actual ports.
func New(identities agentmeta.Catalog, bindings []Integration) (*Registry, error) {
	if identities == nil || nilImplementation(identities) {
		return nil, fmt.Errorf("missing identity catalog")
	}
	r := &Registry{bindings: make(map[agentmeta.ID]Integration), supporting: make(map[agentmeta.Operation][]Integration)}
	for _, b := range bindings {
		d, ok := identities.Lookup(string(b.Descriptor.ID))
		if !ok || d.ID != b.Descriptor.ID {
			return nil, fmt.Errorf("unknown canonical binding %q", b.Descriptor.ID)
		}
		if _, ok := r.bindings[d.ID]; ok {
			return nil, fmt.Errorf("duplicate binding %q", d.ID)
		}
		if b.Launcher != nil && nilImplementation(b.Launcher) || b.Runtime != nil && nilImplementation(b.Runtime) || b.Hooks != nil && nilImplementation(b.Hooks) || b.Decoder != nil && nilImplementation(b.Decoder) {
			return nil, fmt.Errorf("agent %s has a typed-nil implementation", d.ID)
		}
		if b.Launcher == nil && b.Runtime == nil && b.Hooks == nil && b.Decoder == nil {
			return nil, fmt.Errorf("agent %s has no operations", d.ID)
		}
		// Declaration metadata and operation promises cannot override the catalog.
		if len(b.Descriptor.Aliases) > 0 || b.Descriptor.DisplayName != "" || len(b.Descriptor.Operations) > 0 {
			return nil, fmt.Errorf("binding %s must contain only its canonical ID", d.ID)
		}
		d.Operations = nil
		if b.Launcher != nil {
			d.Operations = append(d.Operations, agentmeta.Launch)
		}
		if b.Runtime != nil {
			d.Operations = append(d.Operations, agentmeta.Runtime)
		}
		if b.Hooks != nil {
			d.Operations = append(d.Operations, agentmeta.ManagedHooks)
		}
		if b.Decoder != nil {
			d.Operations = append(d.Operations, agentmeta.LifecycleHooks)
		}
		b.Descriptor = d
		r.bindings[d.ID] = b
	}
	ds := identities.All()
	for i, d := range ds {
		b, ok := r.bindings[d.ID]
		if !ok {
			return nil, fmt.Errorf("missing binding %s", d.ID)
		}
		ds[i] = b.Descriptor
		r.addOperations(b)
	}
	var err error
	r.catalog, err = agentmeta.New(ds)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func nilImplementation(v interface{}) bool {
	rv := reflect.ValueOf(v)
	kind := rv.Kind()
	if kind == reflect.Chan || kind == reflect.Func || kind == reflect.Interface || kind == reflect.Map || kind == reflect.Pointer || kind == reflect.Slice {
		return rv.IsNil()
	}
	return false
}

// Catalog returns the immutable derived operation catalog.
func (r *Registry) Catalog() agentmeta.Catalog { return r.catalog }

// Lookup resolves aliases and returns defensive descriptor slices.
func (r *Registry) Lookup(name string) (Integration, bool) {
	d, ok := r.catalog.Lookup(name)
	if !ok {
		return Integration{}, false
	}
	b := r.bindings[d.ID]
	if b.Hooks != nil {
		d.Operations = append(d.Operations, agentmeta.ManagedHooks)
	}
	if b.Decoder != nil {
		d.Operations = append(d.Operations, agentmeta.LifecycleHooks)
	}
	b.Descriptor = d
	return b, true
}

// Supporting returns integrations in stable presentation order with copied metadata.
func (r *Registry) Supporting(op agentmeta.Operation) []Integration {
	out := make([]Integration, 0, len(r.supporting[op]))
	for _, b := range r.supporting[op] {
		entry, _ := r.Lookup(string(b.Descriptor.ID))
		out = append(out, entry)
	}
	return out
}

// NewBuiltins binds the built-in identities to their concrete implementations.
func NewBuiltins() *Registry {
	r, err := New(agentmeta.Builtins(), []Integration{
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Claude}, Launcher: claude.Launcher{}, Runtime: claude.RuntimeDetector{}, Hooks: claude.Hooks(), Decoder: claude.Decoder()},
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Codex}, Launcher: codex.Launcher{}, Runtime: codex.RuntimeDetector{}, Hooks: codex.Hooks(), Decoder: codex.Decoder()},
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Cursor}, Launcher: cursor.Launcher{}, Runtime: cursor.RuntimeDetector{}, Hooks: cursor.Hooks(), Decoder: cursor.Decoder()},
	})
	if err != nil {
		panic(err)
	}
	return r
}

// Launcher resolves the native launch operation without exposing other ports.
func (r *Registry) Launcher(name string) (agentapi.Launcher, bool) {
	b, ok := r.Lookup(name)
	return b.Launcher, ok && b.Launcher != nil
}

// Observations visits the precomputed runtime projection in presentation order.
func (r *Registry) Observations(env agentapi.RuntimeEnvironment) []agentapi.AgentRuntime {
	out := make([]agentapi.AgentRuntime, 0, len(r.runtime))
	for _, b := range r.runtime {
		observation := b.Runtime.Detect(env)
		if observation.PresenceKey != "" {
			out = append(out, agentapi.AgentRuntime{Agent: b.Descriptor.ID, RuntimeObservation: observation})
		}
	}
	return out
}

// SessionEnvironmentKeys returns the defensive cleanup union built at composition.
func (r *Registry) SessionEnvironmentKeys() []string { return slices.Clone(r.sessionKeys) }

// Launchers returns the implemented launch projection in presentation order.
func (r *Registry) Launchers() []agentapi.AgentLauncher {
	bindings := r.Supporting(agentmeta.Launch)
	out := make([]agentapi.AgentLauncher, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, agentapi.AgentLauncher{Agent: b.Descriptor.ID, Launcher: b.Launcher})
	}
	return out
}

func (r *Registry) addOperations(b Integration) {
	for _, op := range b.Descriptor.Operations {
		r.supporting[op] = append(r.supporting[op], b)
	}
	if b.Runtime == nil {
		return
	}
	r.runtime = append(r.runtime, b)
	for _, key := range b.Runtime.SessionEnvironmentKeys() {
		if !slices.Contains(r.sessionKeys, key) {
			r.sessionKeys = append(r.sessionKeys, key)
		}
	}
}

// LookupHooks returns only the managed configuration port.
func (r *Registry) LookupHooks(name string) (agentapi.HookConfigurator, bool) {
	b, ok := r.Lookup(name)
	return b.Hooks, ok && b.Hooks != nil
}

// HookAgents returns stable native configuration owners.
func (r *Registry) HookAgents() []string {
	var names []string
	for _, b := range r.Supporting(agentmeta.ManagedHooks) {
		names = append(names, string(b.Descriptor.ID))
	}
	return names
}

// LookupDecoder resolves a narrow lifecycle port without host operations.
func (r *Registry) LookupDecoder(name string) (agentapi.HookDecoder, bool) {
	b, ok := r.Lookup(name)
	return b.Decoder, ok && b.Decoder != nil
}
