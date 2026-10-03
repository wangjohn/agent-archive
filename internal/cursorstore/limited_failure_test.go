package cursorstore

import (
	"database/sql"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestLimitedComposerPreservesLegacyFormatFailure(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY,value BLOB)`); err != nil {
		t.Fatal(err)
	}
	for name, composer := range map[string]string{
		"malformed JSON":      `{"unknown":"unfinished`,
		"invalid timestamp":   `{"lastUpdatedAt":"invalid","conversation":[]}`,
		"invalid header type": `{"fullConversationHeadersOnly":{}}`,
		"invalid identity":    `{"fullConversationHeadersOnly":[{"bubbleId":7}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.ExecContext(t.Context(), `INSERT OR REPLACE INTO cursorDiskKV VALUES ('composerData:c',?)`, composer); err != nil {
				t.Fatal(err)
			}
			_, _, legacyErr := queryComposer(t.Context(), db, "c")
			if legacyErr == nil || ReasonOf(legacyErr) != UnknownFormat {
				t.Fatalf("legacy refusal: %v", legacyErr)
			}
			// Small injected policies exercise the same preallocation branch
			// without changing native production limits or fixture semantics.
			for _, limits := range [][2]int64{{1, 0}, {0, 1}} {
				_, _, err := queryComposerLimited(t.Context(), db, "c", limits[0], limits[1])
				if err == nil || ReasonOf(err) != ReasonOf(legacyErr) || agentapi.HasFailure(err, agentapi.Limit) {
					t.Errorf("limits=%v: bounded refusal=%v, legacy=%v", limits, err, legacyErr)
				}
			}
		})
	}
}
