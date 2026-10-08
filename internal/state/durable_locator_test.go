package state

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
)

func TestDurableSessionReadLocatorBoundary(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(home, config.Config{MachineID: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	store := OpenReadOnly(home)
	for _, id := range []string{"descriptive-id", "12345678-1234-1234-1234-123456789abc", strings.Repeat("a", 32), "会話-λ", strings.Repeat("a", 250), strings.Repeat("λ", 125)} {
		if err := store.CheckDurableSessionRead(id); err != nil {
			t.Fatalf("supported locator length %d: %v", len(id), err)
		}
	}
	for _, id := range []string{"", "..", "a/b", "a\\b", "a\x00b", "a\nb", "a\x1bb", "a\x7fb", "a\u0085b", strings.Repeat("a", 251), strings.Repeat("λ", 126)} {
		if err := store.CheckDurableSessionRead(id); !errors.Is(err, ErrDurableStorageRecovery) || err.Error() != ErrDurableStorageRecovery.Error() {
			t.Fatalf("invalid locator length %d disclosed detail: %v", len(id), err)
		}
	}
}
