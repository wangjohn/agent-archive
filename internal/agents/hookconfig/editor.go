package hookconfig

import (
	"github.com/wangjohn/agent-archive/internal/jsonedit"
)

type object = jsonedit.Object

type member = jsonedit.Member

// ErrInvalidConfiguration identifies a refused existing JSON document.
var ErrInvalidConfiguration = jsonedit.ErrInvalidConfiguration

var errInvalidConfiguration = ErrInvalidConfiguration

func parseDocument(src []byte) (*jsonedit.Document, error) {
	return jsonedit.Parse(src, "hooks", "version")
}
