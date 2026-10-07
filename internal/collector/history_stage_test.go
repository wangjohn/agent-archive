package collector

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

func decodeHistoryStage(ctx context.Context, metadata archive.Metadata, p state.PendingPublication, ref archive.SourceReference, raw []byte) (archive.SourceBundle, error) {
	scan := &sessionScan{ctx: ctx}
	defer scan.releaseRetained()
	return scan.decodeHistoryStage(metadata, p, ref, raw)
}
