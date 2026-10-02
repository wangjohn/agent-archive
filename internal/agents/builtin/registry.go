// Package builtin composes immutable integrations without host probes.
package builtin

import (
	"fmt"
	"reflect"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/claude"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/agents/cursor"
)

// Integration binds implemented operations to an identity declaration.
type Integration struct {
	Descriptor      agentmeta.Descriptor
	Launcher        agentapi.Launcher
	Hooks           agentapi.HookConfigurator
	Decoder         agentapi.HookDecoder
	Skills          agentapi.SkillProvider
	NativeHeaders   agentapi.NativeHeaderInspector
	Evidence        agentapi.CapabilityEvidenceProvider
	Version         agentapi.VersionInspector
	Discovery       agentapi.Discoverer
	DatabaseCatalog agentapi.DatabaseCatalogInspector
	NativePaths     agentapi.NativePathsProvider
	Worktrees       agentapi.MissingWorktreeResolver
	Workspace       agentapi.WorkspaceResolver
	Children        agentapi.ChildDiscoverer
}

// Registry holds validated immutable lookups and operation projections.
type Registry struct {
	catalog    agentmeta.Catalog
	bindings   map[agentmeta.ID]Integration
	supporting map[agentmeta.Operation][]Integration
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
		if b.Launcher == nil || nilImplementation(b.Launcher) {
			return nil, fmt.Errorf("agent %s has no launcher", d.ID)
		}
		// Declaration metadata and operation promises cannot override the catalog.
		if len(b.Descriptor.Aliases) > 0 || b.Descriptor.DisplayName != "" || len(b.Descriptor.Operations) > 0 {
			return nil, fmt.Errorf("binding %s must contain only its canonical ID", d.ID)
		}
		d.Operations = []agentmeta.Operation{agentmeta.Launch}
		if b.Hooks != nil {
			if nilImplementation(b.Hooks) {
				return nil, fmt.Errorf("typed nil hooks for %s", d.ID)
			}
			d.Operations = append(d.Operations, agentmeta.ManagedHooks)
		}
		if b.Skills != nil {
			if nilImplementation(b.Skills) {
				return nil, fmt.Errorf("typed nil skills for %s", d.ID)
			}
			d.Operations = append(d.Operations, agentmeta.Skills)
		}
		if b.NativeHeaders != nil {
			if nilImplementation(b.NativeHeaders) {
				return nil, fmt.Errorf("typed nil native headers for %s", d.ID)
			}
			d.Operations = append(d.Operations, agentmeta.NativeInspection)
		}
		if b.Version != nil {
			if nilImplementation(b.Version) {
				return nil, fmt.Errorf("typed nil version inspector for %s", d.ID)
			}
			d.Operations = append(d.Operations, agentmeta.VersionInspection)
		}
		if b.Evidence != nil && nilImplementation(b.Evidence) {
			return nil, fmt.Errorf("typed nil evidence provider for %s", d.ID)
		}

		for _, entry := range []struct {
			name string
			port any
		}{{"native paths", b.NativePaths}, {"worktrees", b.Worktrees}, {"workspace", b.Workspace}} {
			if entry.port != nil && nilImplementation(entry.port) {
				return nil, fmt.Errorf("typed nil %s for %s", entry.name, d.ID)
			}
		}
		if b.NativePaths != nil || b.Worktrees != nil || b.Workspace != nil {
			d.Operations = append(d.Operations, agentmeta.NativeProjects)
		}
		if b.Children != nil {
			if nilImplementation(b.Children) {
				return nil, fmt.Errorf("typed nil children for %s", d.ID)
			}
			d.Operations = append(d.Operations, agentmeta.ChildDiscovery)
		}
		if b.DatabaseCatalog != nil {
			if nilImplementation(b.DatabaseCatalog) {
				return nil, fmt.Errorf("typed nil database catalog for %s", d.ID)
			}
			d.Operations = append(d.Operations, agentmeta.DatabaseInspection)
		}
		if b.Discovery != nil {
			if nilImplementation(b.Discovery) {
				return nil, fmt.Errorf("typed nil discovery for %s", d.ID)
			}
			d.Operations = append(d.Operations, agentmeta.HistoricalDiscovery)
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
		for _, op := range b.Descriptor.Operations {
			r.supporting[op] = append(r.supporting[op], b)
		}
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
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Claude}, Launcher: claude.Launcher{}, Hooks: claude.Hooks(), Decoder: claude.Decoder(), Skills: claude.Skills(), Evidence: claude.CapabilityEvidence{}, Version: claude.VersionInspector{}, NativeHeaders: claude.NativeHeaders{}, Discovery: claude.NativeHeaders{}, NativePaths: claude.ProjectEvidence{}, Worktrees: claude.ProjectEvidence{}, Children: claude.Children{}},
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Codex}, Launcher: codex.Launcher{}, Hooks: codex.Hooks(), Decoder: codex.Decoder(), Skills: codex.Skills(), Evidence: codex.CapabilityEvidence{}, Version: codex.VersionInspector{}, NativeHeaders: codex.NativeHeaders{}, Discovery: codex.NativeHeaders{}, NativePaths: codex.ProjectEvidence{}},
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Cursor}, Launcher: cursor.Launcher{}, Hooks: cursor.Hooks(), Decoder: cursor.Decoder(), Skills: cursor.Skills(), Evidence: cursor.CapabilityEvidence{}, Version: cursor.VersionInspector{}, Discovery: cursor.Discovery{}, DatabaseCatalog: cursor.DatabaseCatalogInspector{}, NativePaths: cursor.ProjectEvidence{}, Workspace: cursor.ProjectEvidence{}},
	})
	if err != nil {
		panic(err)
	}
	return r
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

// LookupSkills resolves a native skill provider.
func (r *Registry) LookupSkills(name string) (agentapi.SkillProvider, bool) {
	b, ok := r.Lookup(name)
	return b.Skills, ok && b.Skills != nil
}

// SkillAgents lists implemented skill providers in catalog order.
func (r *Registry) SkillAgents() []string {
	var out []string
	for _, b := range r.Supporting(agentmeta.Skills) {
		out = append(out, string(b.Descriptor.ID))
	}
	return out
}

// LookupNativeHeaders resolves bounded read-only identity interpretation.
func (r *Registry) LookupNativeHeaders(name string) (agentapi.NativeHeaderInspector, bool) {
	b, ok := r.Lookup(name)
	return b.NativeHeaders, ok && b.NativeHeaders != nil
}

// NativeHeaderAgents lists bound inspectors without probing the host.
func (r *Registry) NativeHeaderAgents() []string {
	var out []string
	for _, d := range r.catalog.All() {
		if _, ok := r.LookupNativeHeaders(string(d.ID)); ok {
			out = append(out, string(d.ID))
		}
	}
	return out
}

// LookupCapabilityEvidence resolves declared support evidence independently of health.
func (r *Registry) LookupCapabilityEvidence(name string) (agentapi.CapabilityEvidenceProvider, bool) {
	b, ok := r.Lookup(name)
	return b.Evidence, ok && b.Evidence != nil
}

// LookupVersionInspector resolves native installed-version observation.
func (r *Registry) LookupVersionInspector(name string) (agentapi.VersionInspector, bool) {
	b, ok := r.Lookup(name)
	return b.Version, ok && b.Version != nil
}

// VersionAgents lists bound inspectors without eager probes.
func (r *Registry) VersionAgents() []string {
	var names []string
	for _, d := range r.catalog.All() {
		if _, ok := r.LookupVersionInspector(string(d.ID)); ok {
			names = append(names, string(d.ID))
		}
	}
	return names
}

// LookupDiscovery resolves native read-only discovery.
func (r *Registry) LookupDiscovery(name string) (agentapi.Discoverer, bool) {
	b, ok := r.Lookup(name)
	return b.Discovery, ok && b.Discovery != nil
}

// DiscoveryAgents lists actual discovery bindings in catalog order.
func (r *Registry) DiscoveryAgents() []string {
	var names []string
	for _, d := range r.catalog.All() {
		if _, ok := r.LookupDiscovery(string(d.ID)); ok {
			names = append(names, string(d.ID))
		}
	}
	return names
}

// LookupDatabaseCatalog resolves native compact catalog interpretation without opening a database.
func (r *Registry) LookupDatabaseCatalog(name string) (agentapi.DatabaseCatalogInspector, bool) {
	b, ok := r.Lookup(name)
	return b.DatabaseCatalog, ok && b.DatabaseCatalog != nil
}

func (r *Registry) LookupNativePaths(name string) (agentapi.NativePathsProvider, bool) {
	b, ok := r.Lookup(name)
	return b.NativePaths, ok && b.NativePaths != nil
}
func (r *Registry) NativePathAgents() []string {
	var out []string
	for _, d := range r.catalog.All() {
		if r.bindings[d.ID].NativePaths != nil {
			out = append(out, string(d.ID))
		}
	}
	return out
}
func (r *Registry) WorktreeResolvers() []agentapi.MissingWorktreeResolver {
	var out []agentapi.MissingWorktreeResolver
	for _, d := range r.catalog.All() {
		if p := r.bindings[d.ID].Worktrees; p != nil {
			out = append(out, p)
		}
	}
	return out
}
func (r *Registry) LookupWorkspace(name string) (agentapi.WorkspaceResolver, bool) {
	b, ok := r.Lookup(name)
	return b.Workspace, ok && b.Workspace != nil
}

// LookupChildren resolves native child discovery without admitting registrations.
func (r *Registry) LookupChildren(name string) (agentapi.ChildDiscoverer, bool) {
	b, ok := r.Lookup(name)
	return b.Children, ok && b.Children != nil
}
