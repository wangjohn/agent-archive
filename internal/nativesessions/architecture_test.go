package nativesessions

import (
	"github.com/wangjohn/agent-archive/internal/testutil/importgraph"
	"testing"
)

func TestNativeReadImportBoundaries(t *testing.T) {
	t.Parallel()
	prefix := "github.com/wangjohn/agent-archive/internal/"
	for _, pkg := range []string{"nativesessions", "transcriptio"} {
		direct, all := importgraph.Imports(t, prefix+pkg)
		for _, imports := range [][]string{direct, all} {
			importgraph.Forbid(t, pkg, imports, "os/exec", "net/http", prefix+"config", prefix+"state", prefix+"storage", prefix+"credentials", prefix+"collector", prefix+"cli", prefix+"terminal")
		}
	}
}
