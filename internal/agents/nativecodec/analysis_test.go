package nativecodec_test

import (
	"bytes"
	"context"

	"encoding/json"

	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func equalJSON(t *testing.T, what string, want, got any) {
	t.Helper()
	a, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("%s differs\nwant %s\ngot  %s", what, a, b)
	}
}

func TestNativeComposerFilterAndAnalysisPreserveGoldenPaths(t *testing.T) {
	paths, err := filepath.Glob("../../archive/testdata/cursor-composer/*.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, ".golden.json") {
			continue
		}
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fixture struct {
				Composer json.RawMessage            `json:"composer"`
				Bubbles  []nativecodec.CursorBubble `json:"bubbles"`
			}
			if err := json.Unmarshal(raw, &fixture); err != nil {
				t.Fatal(err)
			}
			var native struct {
				Composer json.RawMessage            `json:"composer"`
				Bubbles  []nativecodec.CursorBubble `json:"bubbles"`
			}
			if err := json.Unmarshal(raw, &native); err != nil {
				t.Fatal(err)
			}
			want, wantErr := (nativecodec.CursorAdapter{}).FilterComposer(nativecodec.CursorComposer{Composer: fixture.Composer, Bubbles: fixture.Bubbles})
			i := 0
			got, gotErr := nativecodec.FilterComposerRecords(context.Background(), native.Composer, func(context.Context) (nativecodec.CursorBubble, bool, error) {
				if i == len(native.Bubbles) {
					return nativecodec.CursorBubble{}, false, nil
				}
				b := native.Bubbles[i]
				i++
				return b, true, nil
			})
			if (wantErr != nil) != (gotErr != nil) {
				t.Fatalf("filter errors %v / %v", wantErr, gotErr)
			}
			equalJSON(t, "composer filter", want, got)
			if wantErr == nil {
				if !reflect.DeepEqual(want.Records, got.Records) {
					t.Error("retained record bytes changed")
				}
				if want.NativeStartAt != got.NativeStartAt || want.NativeEndAt != got.NativeEndAt || want.NativeStartComplete != got.NativeStartComplete {
					t.Error("local provenance changed")
				}
			}
		})
	}
}
