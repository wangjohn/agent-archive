package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func filterHistory(ctx context.Context, in agentapi.RecordInput) (archive.FilteredTranscript, error) {
	descriptor, more, err := in.Next(ctx)
	if err != nil {
		return archive.FilteredTranscript{}, err
	}
	if !more || descriptor.Kind != agentapi.CodexHistoryHeader || descriptor.History == nil {
		return archive.FilteredTranscript{}, errors.New("history descriptor required")
	}
	history := *descriptor.History
	history.Spans = slices.Clone(history.Spans)
	original := slices.Clone(history.Spans)
	var ordinals []uint64
	var pending agentapi.NativeRecord
	var streamErr error
	span := 0
	rawIndex := 0
	kept := 0
	for i := range history.Spans {
		history.Spans[i].FirstRecord = 0
		history.Spans[i].EndRecord = 0
	}
	out, err := nativecodec.FilterCodexHistory(func() ([]byte, bool) {
		frame, more, e := in.Next(ctx)
		if e != nil {
			streamErr = e
			return nil, false
		}
		if !more {
			return nil, false
		}
		if frame.Kind != agentapi.CodexHistoryRecord || rawIndex >= archive.MaxHistoryRecords {
			streamErr = errors.New("invalid history frame")
			return nil, false
		}
		for span < len(original)-1 && rawIndex >= original[span].EndRecord {
			span++
		}
		if span >= len(original) || frame.Key != original[span].RolloutID || frame.Ordinal < original[span].StartOrdinal || frame.Ordinal >= original[span].EndOrdinal {
			streamErr = errors.New("history frame disagrees with manifest")
			return nil, false
		}
		pending = frame
		rawIndex++
		return frame.Raw, true
	}, func() error { return errors.Join(streamErr, ctx.Err()) }, func(count int) {
		for kept < count {
			ordinals = append(ordinals, pending.Ordinal)
			history.Spans[span].EndRecord++
			kept++
		}
	})
	if err != nil {
		return archive.FilteredTranscript{}, errors.Join(streamErr, err)
	}
	if rawIndex != original[len(original)-1].EndRecord {
		return archive.FilteredTranscript{}, errors.New("incomplete history frames")
	}
	next := 0
	for i := range history.Spans {
		n := history.Spans[i].EndRecord
		history.Spans[i].FirstRecord = next
		next += n
		history.Spans[i].EndRecord = next
	}
	selected, e := (nativecodec.CodexAdapter{}).FilterJSONL(bytes.NewReader(descriptor.Raw))
	if e != nil {
		return archive.FilteredTranscript{}, e
	}
	out.History = &history
	out.Ordinals = ordinals
	out.LocalIdentity = selected.LocalIdentity
	out.SessionIDs = []string{history.ThreadID}
	out.NativeStartAt = selected.NativeStartAt
	out.ObservedHarness = selected.ObservedHarness
	return out, nil
}

type retainedHistory struct {
	bundle archive.SourceBundle
	index  int
	header bool
}

func (r *retainedHistory) Next(ctx context.Context) (agentapi.NativeRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return agentapi.NativeRecord{}, false, err
	}
	if !r.header {
		r.header = true
		last := r.bundle.History.Spans[len(r.bundle.History.Spans)-1]
		for i := last.FirstRecord; i < last.EndRecord; i++ {
			if r.bundle.NativeRecords[i]["type"] == "session_meta" {
				raw, err := json.Marshal(r.bundle.NativeRecords[i])
				return agentapi.NativeRecord{Kind: agentapi.CodexHistoryHeader, History: r.bundle.History, Raw: raw}, true, err
			}
		}
		return agentapi.NativeRecord{}, false, errors.New("selected history metadata missing")
	}
	if r.index == len(r.bundle.NativeRecords) {
		return agentapi.NativeRecord{}, false, nil
	}
	i := r.index
	r.index++
	span, _ := r.bundle.History.SpanAt(i)
	raw, err := json.Marshal(r.bundle.NativeRecords[i])
	return agentapi.NativeRecord{Kind: agentapi.CodexHistoryRecord, Key: span.RolloutID, Raw: raw, Ordinal: r.bundle.Ordinals[i]}, true, err
}

func sameHistory(a, b archive.SourceBundle) bool {
	if a.History == nil || b.History == nil {
		return a.History == nil && b.History == nil
	}
	if a.History.ActiveRolloutID != b.History.ActiveRolloutID || a.History.ThreadID != b.History.ThreadID {
		return false
	}
	if a.History.OwnStart == nil || b.History.OwnStart == nil {
		return a.History.OwnStart == nil && b.History.OwnStart == nil
	}
	return *a.History.OwnStart == *b.History.OwnStart
}
