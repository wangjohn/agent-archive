package state

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

type readablePendingMode string

const (
	readablePendingOrdinary     readablePendingMode = "ordinary-attempted"
	readablePendingHashSpelling readablePendingMode = "hash-case-and-whitespace"
	readablePendingMetadataOnly readablePendingMode = "metadata-only"
	readablePendingLegacy       readablePendingMode = "supported-legacy"
)

func TestSupportedPendingReadChecksReleaseSharedScratch(t *testing.T) {
	for _, mode := range []readablePendingMode{readablePendingOrdinary, readablePendingHashSpelling, readablePendingMetadataOnly, readablePendingLegacy} {
		t.Run(string(mode), func(t *testing.T) {
			s := newTestStore(t)
			source := []byte("synthetic source")
			p := PendingPublication{SourceKey: "source", MetadataKey: "metadata", SourceSHA256: durableRef(source).SHA256, SourceBytes: source, MetadataBytes: []byte(`{}`), Attempted: true}
			switch mode {
			case readablePendingHashSpelling:
				p.SourceSHA256 = " \n" + strings.ToUpper(p.SourceSHA256) + "\t "
			case readablePendingMetadataOnly:
				p.MetadataOnly, p.SourceSize, p.SourceBytes = true, len(source), nil
				// A metadata-only read does not prove or read the remote source.
				p.SourceSHA256 = "existing-remote-reference"
			case readablePendingOrdinary, readablePendingLegacy:
			}
			if mode == readablePendingLegacy {
				raw, err := json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				if err := local.WriteBytes(s.pendingPath("supported"), raw); err != nil {
					t.Fatal(err)
				}
			} else if err := s.SavePending("supported", p); err != nil {
				t.Fatal(err)
			}
			budget := agentapi.NewNativeReadBudget(1 << 20)
			scoped, closeScope := s.WithReadBudget(t.Context(), budget)
			defer closeScope()
			available := budget.Available()
			for range 3 {
				if err := scoped.CheckDurableSessionRead("supported"); err != nil {
					t.Fatal(err)
				}
				if scoped.resourceBudget != budget || budget.Available() != available {
					t.Fatalf("discarded read retained shared capacity: %d want %d", budget.Available(), available)
				}
			}
			borrow, closeBorrow := scoped.WithReadBudget(t.Context(), budget)
			got, found, err := borrow.LoadPending("supported")
			supported := err == nil && found && got.Attempted == p.Attempted && got.CarriesNoSource() == p.CarriesNoSource() && got.SourceSHA256 == p.SourceSHA256
			closeBorrow()
			if !supported || budget.Available() != available {
				t.Fatalf("supported read refused or leaked: found=%t err=%v available=%d", found, err, budget.Available())
			}
			cfg, found, err := config.Load(s.home)
			if err != nil || !found || cfg.DurableStorageProtection != (mode != readablePendingLegacy) {
				t.Fatalf("read changed durable floor: %+v found=%t err=%v", cfg, found, err)
			}
		})
	}
}
