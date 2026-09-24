package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

// syntheticChat is one Cursor chat the composer filter accepts, as
// ReadChat returns it: its composerData value, with extra merged in (a nil
// value removes the field), and one message per text, the person's first,
// then alternating with the assistant's.
func syntheticChat(id string, extra map[string]any, texts ...string) cursorstore.Composer {
	created := time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC).UnixMilli()
	var headers []map[string]any
	c := cursorstore.Composer{}
	for i, text := range texts {
		bubbleID := fmt.Sprintf("%s-m%d", id, i)
		kind := 1 + i%2
		headers = append(headers, map[string]any{"bubbleId": bubbleID, "type": kind})
		row, _ := json.Marshal(map[string]any{"_v": 3, "bubbleId": bubbleID, "type": kind, "text": text, "createdAt": created + int64(i)*1000})
		c.Bubbles = append(c.Bubbles, cursorstore.Bubble{ID: bubbleID, Value: row})
	}
	v := map[string]any{"_v": 18, "composerId": id, "createdAt": created, "lastUpdatedAt": created + int64(len(texts))*1000,
		"status": "completed", "fullConversationHeadersOnly": headers}
	for k, x := range extra {
		if x == nil {
			delete(v, k)
		} else {
			v[k] = x
		}
	}
	c.Composer, _ = json.Marshal(v)
	return c
}

// chatRows are syntheticChat's rows as state.vscdb holds them.
func chatRows(id string, extra map[string]any, texts ...string) map[string]any {
	c := syntheticChat(id, extra, texts...)
	rows := map[string]any{"composerData:" + id: string(c.Composer)}
	for _, b := range c.Bubbles {
		rows["bubbleId:"+id+":"+b.ID] = string(b.Value)
	}
	return rows
}

// mergeRows merges row maps.
func mergeRows(sets ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, s := range sets {
		maps.Copy(out, s)
	}
	return out
}

// fakeCursorDatabase is an Environment.CursorDatabase over chats held in
// memory: listed as given, and read whole from composers.
func fakeCursorDatabase(chats []CursorDatabaseChat, composers map[string]cursorstore.Composer, subagents map[string][]string, calls *int) func(context.Context) (CursorDatabaseResult, error) {
	return func(context.Context) (CursorDatabaseResult, error) {
		if calls != nil {
			*calls++
		}
		for i := range chats {
			if chats[i].KeyID == "" {
				chats[i].KeyID = chats[i].ID
			}
		}
		return CursorDatabaseResult{
			Checked: true, Chats: chats, Subagents: subagents,
			ReadChat: func(_ context.Context, id string) (cursorstore.Composer, error) {
				c, ok := composers[id]
				if !ok {
					return cursorstore.Composer{}, cursorstore.ErrComposerNotFound
				}
				return c, nil
			},
			Close: func() error { return nil },
		}, nil
	}
}

// databaseCandidates are the plan's candidates read from Cursor's database.
func databaseCandidates(p Plan) []Candidate {
	var out []Candidate
	for _, c := range p.Candidates {
		if c.SourceKind == archive.SourceKindCursorSQLite {
			out = append(out, c)
		}
	}
	return out
}

// databaseOutcomes counts the database candidates by skip reason; "" is
// imported.
func databaseOutcomes(p Plan) map[SkipReason]int {
	out := map[SkipReason]int{}
	for _, c := range databaseCandidates(p) {
		out[c.Skip]++
	}
	return out
}
