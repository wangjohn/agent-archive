package cursor

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourceio"
	"time"
)

// Filter selects only the previously accepted Cursor file and composer formats.
type Filter struct{ archive.CursorAdapter }

// Filter consumes verified native input under shared collection limits.
func (f Filter) Filter(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext) (archive.FilteredTranscript, error) {
	if in.File != nil {
		out, err := sourceio.FilterJSONL(ctx, in, c, f.FilterJSONL)
		if err == nil || !agentapi.Deterministic(err) || agentapi.HasFailure(err, agentapi.Unsafe) || agentapi.HasFailure(err, agentapi.Limit) || agentapi.Failure(err) != agentapi.FormatMismatch {
			return out, err
		}
		limit := c.Limits.RecordBytes
		if limit <= 0 {
			limit = archive.MaxRecordBytes
		}
		if in.File.Length() > limit {
			return archive.FilteredTranscript{}, agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge)
		}
		out, err = f.FilterText(sourceio.Reader(ctx, in.File, in.File.Length()), c.StartedAt)
		if err != nil {
			return out, errors.Join(sourceio.Classify(err), sourceio.Classify(in.File.Check()), ctx.Err())
		}
		return out, errors.Join(sourceio.Classify(in.File.Check()), ctx.Err())
	}
	if in.Records == nil {
		return archive.FilteredTranscript{}, errors.New("cursor native input required")
	}
	var composer archive.CursorComposer
	for {
		r, ok, err := in.Records.Next(ctx)
		if err != nil {
			return archive.FilteredTranscript{}, err
		}
		if !ok {
			break
		}
		switch r.Kind {
		case agentapi.ComposerRecord:
			composer.Composer = r.Raw
		case agentapi.BubbleRecord:
			composer.Bubbles = append(composer.Bubbles, archive.CursorBubble{ID: r.Key, Value: r.Raw})
		default:
			return archive.FilteredTranscript{}, agentapi.Wrap(agentapi.Unsafe, errors.New("unexpected Cursor record framing"))
		}
	}
	out, err := f.FilterComposer(composer)
	return out, errors.Join(sourceio.Classify(err), ctx.Err())
}

// Refilter applies current privacy rules to retained native evidence.
func (f Filter) Refilter(ctx context.Context, b archive.SourceBundle, at time.Time) (archive.FilteredTranscript, error) {
	if len(b.NativeText) > 0 {
		return sourceio.RefilterText(ctx, b, at, f.FilterText)
	}
	return sourceio.RefilterJSONL(ctx, f, b)
}
