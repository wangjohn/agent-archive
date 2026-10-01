// Package filechange holds value-only plans for shared atomic file application.
package filechange

import "io/fs"

// Change describes expected prior bytes and the replacement of one local file.
// Native integrations prepare values; shared orchestration owns every I/O effect.
type Change struct {
	Path    string
	Before  []byte
	After   []byte
	Existed bool
	Mode    fs.FileMode
	Delete  bool `json:",omitempty"`
}
