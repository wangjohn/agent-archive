// Package agentmeta owns pure agent identities and presentation metadata.
package agentmeta

import (
	"fmt"
	"slices"
	"strings"
)

// ID is a canonical, safe archive key component.
type ID string

// Built-in canonical identities are shared by legacy archive helpers.
const (
	Claude ID = "claude"
	Codex  ID = "codex"
	Cursor ID = "cursor"
)

// Operation names an implemented integration port, not host health or evidence.
type Operation string

// Launch means an integration implements native launch argument construction.
const Launch Operation = "launch"
const Parse Operation = "parse"

// Source means an integration provides bounded sources and native filtering.
const Source Operation = "source"

// Runtime means an integration implements native runtime observation.
const Runtime Operation = "runtime"

// ManagedHooks means pure hook planning and inspection are implemented.
const ManagedHooks Operation = "managed-hooks"

// LifecycleHooks means native hook decoding is implemented.
const LifecycleHooks Operation = "lifecycle-hooks"

// Descriptor describes one identity. Operations are populated by composition.
type Descriptor struct {
	ID          ID
	Aliases     []string
	DisplayName string
	Operations  []Operation
}

// Catalog resolves external names and lists identities in presentation order.
type Catalog interface {
	Lookup(string) (Descriptor, bool)
	All() []Descriptor
}

type catalog struct {
	descriptors []Descriptor
	names       map[string]int
}

// New constructs an immutable catalog, rejecting ambiguous or unsafe names.
func New(descriptors []Descriptor) (Catalog, error) {
	c := &catalog{names: make(map[string]int)}
	for _, d := range descriptors {
		if !safe(string(d.ID)) || d.DisplayName == "" {
			return nil, fmt.Errorf("invalid agent descriptor %q", d.ID)
		}
		for _, name := range append([]string{string(d.ID)}, d.Aliases...) {
			if !safe(name) {
				return nil, fmt.Errorf("invalid agent name %q", name)
			}
			if _, ok := c.names[name]; ok {
				return nil, fmt.Errorf("duplicate agent name %q", name)
			}
			c.names[name] = len(c.descriptors)
		}
		seen := map[Operation]bool{}
		for _, op := range d.Operations {
			if (op != Parse && op != Launch && op != Runtime && op != ManagedHooks && op != LifecycleHooks && op != Source) || seen[op] {
				return nil, fmt.Errorf("invalid operation %q for %s", op, d.ID)
			}
			seen[op] = true
		}
		c.descriptors = append(c.descriptors, clone(d))
	}
	return c, nil
}

func safe(name string) bool {
	if name == "" || len(name) > 64 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for _, ch := range name {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '-':
		default:
			return false
		}
	}
	return true
}

func clone(d Descriptor) Descriptor {
	d.Aliases = slices.Clone(d.Aliases)
	d.Operations = slices.Clone(d.Operations)
	return d
}

func (c *catalog) Lookup(name string) (Descriptor, bool) {
	i, ok := c.names[Normalize(name)]
	if !ok {
		return Descriptor{}, false
	}
	return clone(c.descriptors[i]), true
}

func (c *catalog) All() []Descriptor {
	out := make([]Descriptor, len(c.descriptors))
	for i, d := range c.descriptors {
		out[i] = clone(d)
	}
	return out
}

// Normalize preserves unknown archived names after trimming and lowercasing.
func Normalize(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// Canonical resolves a catalog alias, preserving unknown normalized names.
func Canonical(c Catalog, name string) string {
	// Pure archive ingress needs only the ID, not a copied descriptor.
	if concrete, ok := c.(*catalog); ok {
		name = Normalize(name)
		if i, known := concrete.names[name]; known {
			return string(concrete.descriptors[i].ID)
		}
		return name
	}
	if d, ok := c.Lookup(name); ok {
		return string(d.ID)
	}
	return Normalize(name)
}

// Names projects canonical names once for a consumer's known-agent probes.
func Names(c Catalog) []string {
	ds := c.All()
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = string(d.ID)
	}
	return out
}

// SetupNames preserves the established setup presentation: Codex first.
func SetupNames(c Catalog) []string {
	names := Names(c)
	if i := slices.Index(names, string(Codex)); i > 0 {
		names = slices.Concat(names[i:i+1], names[:i], names[i+1:])
	}
	return names
}

var identities = mustCatalog([]Descriptor{
	{ID: Claude, Aliases: []string{"claude-code"}, DisplayName: "Claude Code"},
	{ID: Codex, DisplayName: "Codex"},
	{ID: Cursor, DisplayName: "Cursor"},
})

func mustCatalog(ds []Descriptor) Catalog {
	c, err := New(ds)
	if err != nil {
		panic(err)
	}
	return c
}

// Builtins is the immutable identity-only catalog; construction performs no I/O.
func Builtins() Catalog { return identities }
