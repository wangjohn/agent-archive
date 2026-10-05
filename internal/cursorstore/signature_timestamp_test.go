package cursorstore

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestSignatureTimestampMatchesLegacyNumbers(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY,value BLOB)`); err != nil {
		t.Fatal(err)
	}
	for name, fields := range map[string]string{
		"positive overflow":                       `"lastUpdatedAt":9223372036854775808`,
		"negative overflow":                       `"lastUpdatedAt":-9223372036854775809`,
		"unsigned maximum":                        `"lastUpdatedAt":18446744073709551615`,
		"large finite integer":                    `"lastUpdatedAt":` + strings.Repeat("9", 300),
		"positive nonfinite":                      `"lastUpdatedAt":` + strings.Repeat("9", 400),
		"negative nonfinite":                      `"lastUpdatedAt":-` + strings.Repeat("9", 400),
		"nonfinite exponent":                      `"lastUpdatedAt":1e400`,
		"integer rounding":                        `"lastUpdatedAt":9007199254740993`,
		"real":                                    `"lastUpdatedAt":7.9`,
		"last duplicate wins":                     `"lastUpdatedAt":"invalid","lastUpdatedAt":9223372036854775808`,
		"createdAt ignored":                       `"createdAt":9223372036854775808`,
		"createdAt cannot replace invalid update": `"createdAt":7,"lastUpdatedAt":"invalid"`,
		"update independent of creation":          `"lastUpdatedAt":-9223372036854775809,"createdAt":"invalid"`,
	} {
		t.Run(name, func(t *testing.T) {
			composer := `{` + fields + `,"conversation":[{"bubbleId":"last"}]}`
			want, _, wantErr := decodeHeaders([]byte(composer))
			if _, err := db.ExecContext(t.Context(), `INSERT OR REPLACE INTO cursorDiskKV VALUES ('composerData:c',?)`, composer); err != nil {
				t.Fatal(err)
			}
			for _, read := range []struct {
				name string
				fn   func() (Signature, error)
			}{
				{"SQL", func() (Signature, error) { return signatureOnly(t.Context(), db, "c") }},
				{"streamed", func() (Signature, error) { return signatureJSONFallback(t.Context(), db, "composerData:c", "c") }},
			} {
				got, err := read.fn()
				if (err == nil) != (wantErr == nil) || wantErr == nil && got != want || wantErr != nil && ReasonOf(err) != ReasonOf(wantErr) {
					t.Errorf("%s signature=%+v error=%v, legacy=%+v error=%v", read.name, got, err, want, wantErr)
				}
			}
		})
	}
}

func TestOversizedIntegralTimestampRetainsLimitObservation(t *testing.T) {
	useTempSnapshots(t)
	for _, timestamp := range []string{"9223372036854775808", "-9223372036854775809"} {
		t.Run(timestamp, func(t *testing.T) {
			prefix := `{"lastUpdatedAt":` + timestamp + `,"conversation":[{"bubbleId":"last"}]`
			want, _, err := decodeHeaders([]byte(prefix + `}`))
			if err != nil {
				t.Fatal(err)
			}
			path := StateDatabase(t.TempDir())
			composer := prefix + `,"unknown":"` + strings.Repeat("x", archive.MaxRecordBytes) + `"}`
			writeDB(t, path, false, map[string]any{"composerData:c": composer})
			got, err := ReadSignature(t.Context(), path, "c")
			if err != nil || got != want {
				t.Fatalf("oversized signature=%+v error=%v, legacy=%+v", got, err, want)
			}
			r := NewReader(path)
			defer closeOrFail(t, r)
			_, _, err = r.ReadComposerLimited(t.Context(), "c", 0, archive.MaxRecordBytes)
			observed, ok := agentapi.ErrorObservation(err)
			if !errors.Is(err, ErrRecordLimit) || !ok || observed.Signature != want.SourceSignature() || !agentapi.Deterministic(err) {
				t.Fatalf("lost stable limit observation: %+v found=%v error=%v", observed, ok, err)
			}
		})
	}
}
