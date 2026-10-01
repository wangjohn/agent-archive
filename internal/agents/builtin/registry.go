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
	Descriptor agentmeta.Descriptor
	Launcher   agentapi.Launcher
	Parser     agentapi.TranscriptParser
	Preview    agentapi.RecordPreviewer
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
		if b.Parser != nil {
			if nilImplementation(b.Parser) {
				return nil, fmt.Errorf("agent %s has typed-nil parser", d.ID)
			}
			d.Operations = append(d.Operations, agentmeta.Parse)
		}
		if b.Preview != nil && nilImplementation(b.Preview) {
			return nil, fmt.Errorf("agent %s has typed-nil preview", d.ID)
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
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Claude}, Launcher: claude.Launcher{}, Parser: claude.Parser{}, Preview: claude.Previewer{}},
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Codex}, Launcher: codex.Launcher{}, Parser: codex.Parser{}, Preview: codex.Previewer{}},
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Cursor}, Launcher: cursor.Launcher{}, Parser: cursor.Parser{}},
	})
	if err != nil {
		panic(err)
	}
	return r
}

// LookupParser resolves only the parser needed by retained-source derivation.
func (r *Registry) LookupParser(name string) (agentapi.TranscriptParser, bool) {
	b, ok := r.Lookup(name)
	return b.Parser, ok && b.Parser != nil
}

// LookupPreview resolves a bounded safe-record preview decoder.
func (r *Registry) LookupPreview(name string) (agentapi.RecordPreviewer, bool) {
	b, ok := r.Lookup(name)
	return b.Preview, ok && b.Preview != nil
}
