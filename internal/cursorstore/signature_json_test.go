package cursorstore

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestDeepSignatureStreamsAndDiscardsUnknownContent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	composer := `{"lastUpdatedAt":7,"conversation":[{"bubbleId":"last","unknown":"` + strings.Repeat("x", 2<<20) + `"}],"deep":` + strings.Repeat("[", 1001) + `0` + strings.Repeat("]", 1001) + `}`
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY,value BLOB); INSERT INTO cursorDiskKV VALUES ('composerData:c',?)`, composer); err != nil {
		t.Fatal(err)
	}
	q := &signatureChunkQueries{querier: db}
	got, err := signatureJSONFallback(t.Context(), q, "composerData:c", "c")
	if err != nil || got != (Signature{LastUpdatedAt: 7, HeaderCount: 1, LastBubbleID: "last"}) {
		t.Fatalf("streamed signature: %+v %v", got, err)
	}
	if q.unbounded || q.chunks != (len(composer)+65535)/65536+1 {
		t.Fatalf("unbounded=%v chunk reads=%d", q.unbounded, q.chunks)
	}
}

type signatureChunkQueries struct {
	querier
	chunks    int
	unbounded bool
}

func (q *signatureChunkQueries) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if query == `SELECT substr(CAST(value AS BLOB),?,65536) FROM cursorDiskKV WHERE key = ?` {
		q.chunks++
	} else {
		q.unbounded = true
	}
	return q.querier.QueryRowContext(ctx, query, args...)
}
