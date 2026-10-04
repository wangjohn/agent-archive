package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestRecoveryRegistrationBufferPreservesWholeDocumentRead(t *testing.T) {
	reg := migrationRegistration(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "native"}, "owner")
	data, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		data    []byte
		missing bool
	}{
		{name: "valid", data: data},
		{name: "trailing-value", data: append(append([]byte{}, data...), []byte(" {}")...)},
		{name: "truncated", data: data[:len(data)-1]},
		{name: "omitted-fields", data: []byte(`{"archive_session_id":"other"}`)},
		{name: "missing", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "registration.json")
			if !tc.missing {
				if err := os.WriteFile(path, tc.data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			var want, got archive.SessionRegistration
			var buffer bytes.Buffer
			oldErr := local.Read(path, &want)
			newErr := readRecoveryRegistration(path, &buffer, &got)
			if !reflect.DeepEqual(want, got) || reflect.TypeOf(oldErr) != reflect.TypeOf(newErr) || recoveryReadErrorText(oldErr) != recoveryReadErrorText(newErr) || errors.Is(oldErr, os.ErrNotExist) != errors.Is(newErr, os.ErrNotExist) {
				t.Fatalf("read differs: old=%#v %v new=%#v %v", want, oldErr, got, newErr)
			}
		})
	}
}

func TestRecoveryRegistrationBufferReleasesOversizedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registration.json")
	large := []byte(`{"ignored":"` + strings.Repeat("x", 128*1024) + `"}`)
	if err := os.WriteFile(path, large, 0600); err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	var reg archive.SessionRegistration
	if err := readRecoveryRegistration(path, &buffer, &reg); err != nil {
		t.Fatal(err)
	}
	if buffer.Cap() <= 64*1024 {
		t.Fatal("fixture did not allocate oversized input")
	}
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	reg = archive.SessionRegistration{}
	if err := readRecoveryRegistration(path, &buffer, &reg); err != nil {
		t.Fatal(err)
	}
	if buffer.Cap() > 64*1024 {
		t.Fatalf("oversized input retained: %d", buffer.Cap())
	}
}

func recoveryReadErrorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestRecoveryDirectoryFingerprintPreservesNameEncoding(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"owner-z.json", "owner-a.json", "line\nquote\".json", "unicode-雪.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	if want, got := phaseFingerprint(names), phaseFingerprint(entries); want != got {
		t.Fatalf("phase fingerprint changed: got %s want %s", got, want)
	}
}
