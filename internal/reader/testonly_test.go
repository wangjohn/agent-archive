package reader

import (
	"context"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Helpers only tests use, kept out of the production files so deadcode
// (golang.org/x/tools/cmd/deadcode) reports only code that is really dead.

// ListMetadata reads only metadata sidecars and applies filters without
// downloading transcript bundles.
func ListMetadata(ctx context.Context, store storage.ObjectStore, prefix string, filter Filter) ([]archive.Metadata, error) {
	return ListMetadataWithOptions(ctx, store, prefix, filter, ListOptions{})
}
