package cursorstore

import (
	"context"
	"database/sql"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"os"
	"strings"
	"testing"
)

func TestBoundedComposerPreservesMissingAndNullRows(t *testing.T) {
	useTempSnapshots(t)
	path := StateDatabase(t.TempDir())
	rows := toAny(chatRows())
	rows["bubbleId:c:b2"] = nil
	writeDB(t, path, false, rows)
	r := NewReader(path)
	defer closeOrFail(t, r)
	c, sig, err := r.ReadComposerLimited(context.Background(), "c", 1<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if sig.MessageRows != 3 || c.Bubbles[1].Missing || c.Bubbles[1].Value != nil {
		t.Fatalf("null row confused with missing: %+v %+v", c.Bubbles[1], sig)
	}
	if _, _, err = r.ReadComposerLimited(context.Background(), "c", 1, 1<<20); !errors.Is(err, agentapi.ErrRawLimit) {
		t.Fatalf("raw limit: %v", err)
	}
	if _, _, err = r.ReadComposerLimited(context.Background(), "c", 1<<20, 1); !errors.Is(err, ErrRecordLimit) {
		t.Fatalf("record limit: %v", err)
	}
}

func TestFailedSnapshotPreparationAttemptIsReused(t *testing.T) {
	root := useTempSnapshots(t)
	path := StateDatabase(t.TempDir())
	startWriter(t, path).put(chatRows())
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	r := NewReader(path)
	defer closeOrFail(t, r)
	first := error(nil)
	for _, id := range []string{"c", "c2"} {
		_, _, err := r.ReadComposerLimited(context.Background(), id, 1<<20, 1<<20)
		if err == nil {
			t.Fatal("snapshot preparation unexpectedly succeeded")
		}
		if first == nil {
			first = err
		} else if !errors.Is(err, first) {
			t.Fatal("snapshot failure was not reused")
		}
	}
	if r.Attempts() != 1 || r.Snapshots() != 0 {
		t.Fatalf("attempts %d backups %d", r.Attempts(), r.Snapshots())
	}
}

func TestSnapshotRemovalFailureStaysVisible(t *testing.T) {
	root := useTempSnapshots(t)
	path := StateDatabase(t.TempDir())
	startWriter(t, path).put(chatRows())
	r := NewReader(path)
	if _, _, err := r.ReadComposerLimited(context.Background(), "c", 1<<20, 1<<20); err != nil {
		t.Fatal(err)
	}
	fault := errors.New("synthetic snapshot removal failure")
	r.hooks.removeSnapshot = func(string) error { return fault }
	if err := r.Close(); !errors.Is(err, fault) {
		t.Fatalf("lost cleanup error: %v", err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
}

func TestSignatureOnlyMatchesLegacyHeaderDecoding(t *testing.T) {
	for name, composer := range map[string]string{
		"header case":            `{"fullConversationHeadersOnly":[{"BubbleID":"b1","bubbleid":"b3"}]}`,
		"duplicates":             `{"lastUpdatedAt":1,"lastUpdatedAt":2,"fullConversationHeadersOnly":[{"bubbleId":"b1","bubbleId":"b3"}]}`,
		"inline":                 `{"lastUpdatedAt":3,"fullConversationHeadersOnly":null,"conversation":[{"bubbleId":"old","text":"omitted"},{}]}`,
		"missing":                `{"lastUpdatedAt":-1,"fullConversationHeadersOnly":[{"bubbleId":"b1"},{"bubbleId":"absent"}]}`,
		"invalid":                `{"lastUpdatedAt":"soon"}`,
		"invalid UTF8 identity":  "{\"fullConversationHeadersOnly\":[{\"bubbleId\":\"invalid-\xff\xfe\"}]}",
		"surrogate identity":     `{"fullConversationHeadersOnly":[{"bubbleId":"invalid-\ud800"}]}`,
		"invalid duplicate type": `{"fullConversationHeadersOnly":[{"bubbleId":7,"bubbleId":"b1"}]}`,
		"huge number":            `{"lastUpdatedAt":1e400}`,
		"raw NUL tail":           "{}\x00",
		"escaped NUL identity":   `{"fullConversationHeadersOnly":[{"bubbleId":"escaped-\u0000"}]}`,
		"deep unknown field":     `{"lastUpdatedAt":7,"unknown":` + strings.Repeat("[", 1001) + `0` + strings.Repeat("]", 1001) + `}`,
		"deep inline field":      `{"conversation":[{"bubbleId":"last","unknown":` + strings.Repeat("[", 1001) + `0` + strings.Repeat("]", 1001) + `}]}`,
		"Go depth boundary":      `{"unknown":` + strings.Repeat("[", 9999) + `0` + strings.Repeat("]", 9999) + `}`,
		"Go depth exceeded":      `{"unknown":` + strings.Repeat("[", 10000) + `0` + strings.Repeat("]", 10000) + `}`,
		"long timestamp":         `{"lastUpdatedAt":0.` + strings.Repeat("0", 2000) + `1e2001,"unknown":` + strings.Repeat("[", 1001) + `0` + strings.Repeat("]", 1001) + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := StateDatabase(t.TempDir())
			rows := toAny(chatRows())
			rows["composerData:c"] = composer
			writeDB(t, path, false, rows)
			want, wantErr := ReadSignature(context.Background(), path, "c")
			var got Signature
			err := Read(context.Background(), path, Options{}, func(ctx context.Context, db *sql.DB) error {
				var err error
				got, err = signatureOnly(ctx, db, "c")
				return err
			})
			if wantErr == nil && err != nil || wantErr != nil && (err == nil || ReasonOf(err) != ReasonOf(wantErr)) || wantErr == nil && got != want {
				t.Fatalf("got %+v %v want %+v %v", got, err, want, wantErr)
			}
		})
	}
}
