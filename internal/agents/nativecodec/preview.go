package nativecodec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"io"
)

// Preview filters a single complete native record and retains only bounded display facts.
func Preview(ctx context.Context, adapter interface {
	FilterJSONL(io.Reader) (archive.FilteredTranscript, error)
}, record []byte) (archive.RecordPreview, error) {
	if err := ctx.Err(); err != nil {
		return archive.RecordPreview{}, err
	}
	filtered, err := adapter.FilterJSONL(bytes.NewReader(record))
	if err != nil && !errors.Is(err, archive.ErrUnsafeSourceFormat) {
		return archive.RecordPreview{}, err
	}
	out := archive.RecordPreview{Gaps: filtered.Gaps}
	for _, encoded := range filtered.Records {
		var safe map[string]any
		if err := json.Unmarshal(encoded, &safe); err != nil {
			return archive.RecordPreview{}, err
		}
		if isSidechainRecord(safe) {
			continue
		}
		if firstString(safe, "type") == "custom-title" {
			out.Name = archive.CollapseSessionTitle(firstString(safe, "customTitle"))
		}
		if branch := archive.ValidBranch(firstStringDeep(safe, "gitBranch")); branch != "HEAD" {
			out.Branch = branch
		}
		out.Activity = parseNativeTimestamp(safe)
		_, text, kind, ok := visibleMessage(safe)
		if ok {
			out.Kind = refineUserKind(safe, kind, text)
			if out.Kind == archive.TurnKindHumanPrompt {
				out.Title = archive.CollapseSessionTitle(text)
			}
			if out.Kind == archive.TurnKindLocalCommand {
				out.Command = archive.CollapseSessionTitle(text)
			}
		}
	}
	return out, nil
}
