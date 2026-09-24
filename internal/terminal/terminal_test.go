package terminal

import (
	"errors"
	"strings"
	"testing"
)

// failingWriter fails every write and counts the attempts.
type failingWriter struct {
	writes int
}

func (w *failingWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, errors.New("terminal closed")
}

func TestHelpersFormatLikeFmt(t *testing.T) {
	var b strings.Builder

	Printf(&b, "%d %s\n", 1, "one")
	Println(&b, "two", 2)
	Print(&b, "three")

	if got, want := b.String(), "1 one\ntwo 2\nthree"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestHelpersIgnoreWriteErrors(t *testing.T) {
	w := &failingWriter{}

	Printf(w, "x")
	Println(w, "x")
	Print(w, "x")

	if w.writes != 3 {
		t.Fatalf("writes = %d, want 3: each helper should attempt its write and carry on", w.writes)
	}
}
