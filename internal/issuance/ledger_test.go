package issuance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func TestLedgerRejectsCorruptAndSymlinkedSlots(t *testing.T) {
	t.Parallel()
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "corrupt", true: "symlink"}[symlink], func(t *testing.T) {
			home := t.TempDir()
			if err := os.Mkdir(filepath.Join(home, "issued"), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, "issued", "slot-"+strings.Repeat("a", 32)+".json")
			if symlink {
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := List(home); err == nil {
				t.Fatal("unsafe ledger accepted")
			}
		})
	}
}

func TestLedgerImmutableIdentityAndStateValidation(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	slot, err := New(strings.Repeat("a", 32), strings.Repeat("b", 64), strings.Repeat("c", 32), cloudflare.BucketRef{Name: "synthetic", Jurisdiction: "eu"}, strings.Repeat("d", 32), Precreated, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(slot.ProviderName) != 118 {
		t.Fatal("provider name limit")
	}
	if err = Save(home, slot); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(home, "issued", "slot-"+slot.SlotID+".json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("ledger must be private")
	}
	slot.State = Spare
	if slot.Validate() == nil {
		t.Fatal("eligible without key ID")
	}
	slot.ProviderID = strings.Repeat("e", 32)
	if slot.Validate() != nil {
		t.Fatal("complete spare refused")
	}
	slot.State = SecretIntent
	if err = Save(home, slot); err != nil {
		t.Fatal(err)
	}
	slot.State = Spare
	if err = Save(home, slot); err != nil {
		t.Fatal(err)
	}
	slot.State = DeliveryIntent
	if slot.Validate() == nil {
		t.Fatal("delivery intent without pairing accepted")
	}
	slot.PairingID = strings.Repeat("f", 32)
	slot.Label = "laptop"
	slot.ExpiresAt = time.Now().Add(time.Hour)
	slot.State = Reserved
	if err = Save(home, slot); err != nil {
		t.Fatal(err)
	}
	slot.State = DeliveryIntent
	if err = Save(home, slot); err != nil {
		t.Fatal(err)
	}
	recycled := slot
	recycled.State = Spare
	recycled.PairingID = ""
	recycled.Label = ""
	recycled.ExpiresAt = time.Time{}
	if Save(home, recycled) == nil {
		t.Fatal("exposed slot recycled")
	}
	changed := slot
	changed.ProviderID = strings.Repeat("f", 32)
	if Save(home, changed) == nil {
		t.Fatal("immutable provider binding replaced")
	}
	slot.ProviderName = "agent-archive bogus"
	if slot.Validate() == nil {
		t.Fatal("changed name accepted")
	}
}
