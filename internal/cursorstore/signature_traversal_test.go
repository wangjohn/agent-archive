package cursorstore

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestSignatureOnlyVisitsComposerInOneStatement(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	composer := `{"lastUpdatedAt":7,"conversation":[{"bubbleId":"last","unknown":{"bubbleId":"` + strings.Repeat("x", 2<<20) + `"}}],"unknown":"` + strings.Repeat("x", 2<<20) + `"}`
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY,value BLOB); INSERT INTO cursorDiskKV VALUES ('composerData:c',?)`, composer); err != nil {
		t.Fatal(err)
	}
	q := &signatureTraversalQueries{querier: db}
	got, err := signatureOnly(t.Context(), q, "c")
	if err != nil || got != (Signature{LastUpdatedAt: 7, HeaderCount: 1, LastBubbleID: "last"}) {
		t.Fatalf("signature=%+v error=%v", got, err)
	}
	if q.rows != 1 || q.scalars != 0 {
		t.Fatalf("composer extraction statements: rows=%d scalars=%d", q.rows, q.scalars)
	}
}

type signatureTraversalQueries struct {
	querier
	rows    int
	scalars int
}

func (q *signatureTraversalQueries) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.rows++
	return q.querier.QueryContext(ctx, query, args...)
}

func (q *signatureTraversalQueries) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.scalars++
	return q.querier.QueryRowContext(ctx, query, args...)
}
