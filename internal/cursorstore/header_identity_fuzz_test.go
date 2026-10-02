package cursorstore

import (
	"database/sql"
	"strings"
	"testing"
)

func FuzzSignatureHeaderConformance(f *testing.F) {
	for _, seed := range []string{
		`{"lastUpdatedAt":7,"fullConversationHeadersOnly":[{"bubbleId":"b"}]}`,
		`{"conversation":[{"bubbleId":"\ud800","text":"omitted"}]}`,
		"{\"fullConversationHeadersOnly\":[{\"bubbleId\":\"\xff\xfe\"}]}",
		`{"fullConversationHeadersOnly":[{"bubbleId":7,"bubbleId":"b"}]}`,
		`{"lastUpdatedAt":1e400}`,
		"{}\x00",
		`{"\u0066\u0075\u006c\u006c\u0043\u006f\u006e\u0076\u0065\u0072\u0073\u0061\u0074\u0069\u006f\u006e\u0048\u0065\u0061\u0064\u0065\u0072\u0073\u004f\u006e\u006c\u0079": [{"bubbleId":"b1"}],"unknown":` + strings.Repeat("[", 1001) + `0` + strings.Repeat("]", 1001) + `}`,
		`{"unknown":` + strings.Repeat("[", 1001) + `0` + strings.Repeat("]", 1001) + `}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, composer string) {
		if len(composer) > 200000 {
			t.Skip()
		}
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(t.Context(), `CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY,value BLOB); INSERT INTO cursorDiskKV VALUES ('composerData:c',?)`, composer); err != nil {
			t.Fatal(err)
		}
		_, want, wantErr := queryComposer(t.Context(), db, "c")
		got, gotErr := signatureOnly(t.Context(), db, "c")
		if (wantErr == nil) != (gotErr == nil) || wantErr == nil && want != got {
			t.Fatalf("SQL signature differs from legacy decoding: got=%+v err=%v want=%+v err=%v", got, gotErr, want, wantErr)
		}
		got, gotErr = signatureJSONFallback(t.Context(), db, "composerData:c", "c")
		if (wantErr == nil) != (gotErr == nil) || wantErr == nil && want != got {
			t.Fatalf("streamed signature differs from legacy decoding: got=%+v err=%v want=%+v err=%v", got, gotErr, want, wantErr)
		}
	})
}
