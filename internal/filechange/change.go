// Package filechange holds value-only plans for shared atomic file application.
package filechange

import "os"

// Change describes expected prior bytes and the replacement of one local file.
// Native integrations prepare values; shared orchestration owns every I/O effect.
type Change struct {
	Path    string
	Before  []byte
	After   []byte
	Existed bool
	Mode    os.FileMode
	Delete  bool `json:",omitempty"`
}
