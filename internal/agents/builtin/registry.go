// Package builtin composes immutable integrations without host probes.
package builtin

import (
	"fmt"
	"reflect"
	"strings"

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
	Sources    agentapi.SourceProvider
	Filter     agentapi.TranscriptFilter
}

// Registry holds validated immutable lookups and operation projections.
type Registry struct {
	catalog        agentmeta.Catalog
	bindings       map[agentmeta.ID]Integration
	sourceBindings map[string]Integration
	supporting     map[agentmeta.Operation][]Integration
}

// New binds each identity exactly once and derives operations from actual ports.
func New(identities agentmeta.Catalog, bindings []Integration) (*Registry, error) {
	if identities == nil || nilImplementation(identities) {
		return nil, fmt.Errorf("missing identity catalog")
	}
	r := &Registry{bindings: make(map[agentmeta.ID]Integration), sourceBindings: map[string]Integration{}, supporting: make(map[agentmeta.Operation][]Integration)}
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
		if b.Sources != nil || b.Filter != nil {
			if b.Sources == nil || b.Filter == nil || nilImplementation(b.Sources) || nilImplementation(b.Filter) {
				return nil, fmt.Errorf("agent %s has incomplete source bindings", d.ID)
			}
			d.Operations = append(d.Operations, agentmeta.Source)
		}
		b.Descriptor = d
		r.bindings[d.ID] = b
		for _, name := range append([]string{string(d.ID)}, d.Aliases...) {
			r.sourceBindings[name] = b
		}
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
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Claude}, Launcher: claude.Launcher{}, Sources: claude.SourceProvider{}, Filter: claude.Filter{}},
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Codex}, Launcher: codex.Launcher{}, Sources: codex.SourceProvider{}, Filter: codex.Filter{}},
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Cursor}, Launcher: cursor.Launcher{}, Sources: cursor.SourceProvider{}, Filter: cursor.Filter{}},
	})
	if err != nil {
		panic(err)
	}
	return r
}

// LookupSources resolves only source and filter ports.
func (r *Registry) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	b, ok := r.sourceBindings[strings.ToLower(strings.TrimSpace(name))]
	return b.Sources, b.Filter, ok && b.Sources != nil && b.Filter != nil
}

// SweepSources invokes only provider-declared abandoned-resource cleanup.
func (r *Registry) SweepSources() {
	for _, b := range r.bindings {
		if p, ok := b.Sources.(interface{ Sweep() }); ok {
			p.Sweep()
		}
	}
}
