package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// answerAfter answers a prompt, running before first (once) as the person
// types: what happens between the plan and the commit.
type answerAfter struct {
	before func()
	answer *strings.Reader
}

func (a *answerAfter) Read(p []byte) (int, error) {
	if a.before != nil {
		a.before()
		a.before = nil
	}
	return a.answer.Read(p)
}

// When the import can't list registrations after confirming (to number its
// batch past every ID they carry), the error says so and carries none of the
// advice about repairing an unreadable file in imports/, which is not the
// problem.
func TestBackfillRegistrationsErrorHasNoImportsAdvice(t *testing.T) {
	f, _ := newImportFixture(t)
	registrations := filepath.Join(f.data, "registrations")
	stdin := &answerAfter{answer: strings.NewReader("y\n"), before: func() {
		if err := os.RemoveAll(registrations); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(registrations, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
	}}
	_, errOut, code := f.importRun(t, stdin, true)
	if code == 0 || !strings.Contains(errOut, "read registrations") || strings.Contains(errOut, "imports") {
		t.Fatalf("code %d: %s", code, errOut)
	}
}
