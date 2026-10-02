package cli

import (
	"os"
	"path/filepath"
	"slices"

	"github.com/wangjohn/agent-archive/internal/hooks"
)

// detectHarnesses best-effort-detects installed applications by checking
// for the configuration directory holding each app's hook file (see
// hooks.ResolveFiles). A directory existing is not proof the application is
// currently installed, and its absence is not proof it isn't; this only
// pre-selects setup's prompts, which the user can override either way.
func detectHarnesses(files hooks.Files) []string {
	var found []string
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if info, err := os.Stat(filepath.Dir(files[name])); err == nil && info.IsDir() {
			found = append(found, name)
		}
	}
	return found
}
