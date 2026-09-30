package cli

import (
	"reflect"
	"testing"
)

// A Ctrl-Z that came while the transcript loaded is answered before the
// pager starts, with the browser's screen put away, instead of stopping
// the process with the alternate screen up and no pager yet.
func TestKeysAnswerAPendingCtrlZBeforeThePager(t *testing.T) {
	t.Parallel()
	fake := newFakeKeys()
	keys := startKeys(fake)
	keys.hide = func() { fake.note("hide") }
	keys.show = func() { fake.note("show") }
	fake.stopPending = true
	end := keys.page()
	end()
	keys.close()
	want := []string{"keys", "lines", "hide", "stop", "keys", "show", "lines", "paging", "paged", "flush", "release"}
	if got := fake.history(); !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

// Ctrl-Z during the wait for an escape sequence's rest is answered as one
// during a read: the keys before it are handled, then the process stops.
func TestKeysCtrlZDuringATimedWait(t *testing.T) {
	t.Parallel()
	fake := newFakeKeys("1\x1b", string(fakeSuspend), "q")
	keys := startKeys(fake)
	defer keys.close()
	var kinds []keyKind
	for range 4 {
		k, err := keys.next()
		if err != nil {
			t.Fatalf("after %v: %v", kinds, err)
		}
		kinds = append(kinds, k.kind)
	}
	if want := []keyKind{keyRune, keyEscape, keyResize, keyRune}; !reflect.DeepEqual(kinds, want) {
		t.Fatalf("keys %v, want %v", kinds, want)
	}
	if got, want := fake.history(), []string{"keys", "lines", "stop", "keys"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("modes %v, want %v", got, want)
	}
}
