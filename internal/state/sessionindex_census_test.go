package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestRecoveryReaderReusesChunkWithoutDecodedState(t *testing.T) {
	s := newTestStore(t)
	reg := migrationRegistration(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "native"}, "owner")
	data, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	path := s.registrationPath("owner")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	reader := s.newRecoveryRegistrationReader()
	defer reader.close()
	for _, input := range [][]byte{
		data,
		[]byte(`{"archive_session_id":"other"}`),
		append(append([]byte{}, data...), []byte(" {}")...),
		[]byte(`{"ignored":"` + strings.Repeat("x", recoveryReadAheadBytes) + `"}`),
		data,
	} {
		if err := os.WriteFile(path, input, 0600); err != nil {
			t.Fatal(err)
		}
		got := reader.readChunk(entries)[0]
		if len(input) > recoveryReadAheadBytes {
			if !errors.Is(got.err, errRecoveryReadAheadLarge) {
				t.Fatalf("large document read ahead: %v", got.err)
			}
			var fallback archive.SessionRegistration
			var buffer bytes.Buffer
			if err := readRecoveryRegistration(path, &buffer, &fallback); err != nil {
				t.Fatal(err)
			}
			continue
		}
		var want archive.SessionRegistration
		wantErr := json.Unmarshal(input, &want)
		if !reflect.DeepEqual(got.reg, want) || reflect.TypeOf(got.err) != reflect.TypeOf(wantErr) {
			t.Fatalf("chunk inherited decoded state: got=%#v %v want=%#v %v", got.reg, got.err, want, wantErr)
		}
	}
}

func TestRecoveryReaderCensusPreservesOrderedFailureAndCancellation(t *testing.T) {
	s := newTestStore(t)
	// Non-JSON files count toward activating read-ahead but never authorize
	// identities or hide an earlier JSON failure.
	for i := range packedSessionIndexThreshold {
		path := filepath.Join(s.home, "registrations", fmt.Sprintf("z%06d.txt", i))
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(s.registrationPath("ignored-directory"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := os.WriteFile(s.registrationPath(id), []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	inventory, err := s.sessionRegistrationInventory(t.Context())
	if inventory != nil || err == nil || !strings.HasPrefix(err.Error(), `read registration "a":`) {
		t.Fatalf("unordered failure: inventory=%v err=%v", inventory, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	inventory, err = s.sessionRegistrationInventory(ctx)
	if inventory != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: inventory=%v err=%v", inventory, err)
	}
}
